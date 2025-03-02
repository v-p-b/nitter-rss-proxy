// Copyright 2021 Daniel Erat.
// All rights reserved.

package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/fcgi"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/feeds"
	"github.com/mmcdole/gofeed"
)

const (
	titleLen    = 80       // max length of title text in feed, in runes
	minIDHeader = "Min-Id" // header returned by Nitter with min ID among items
)

// feedFormat describes different feed formats that can be written.
type feedFormat string

const (
	atomFormat feedFormat = "atom"
	jsonFormat feedFormat = "json"
	rssFormat  feedFormat = "rss"
)

func main() {
	var opts handlerOptions

	addr := flag.String("addr", "localhost:8080", "Network address to listen on")
	base := flag.String("base", "", "Base URL for served feeds")
	flag.BoolVar(&opts.cycle, "cycle", true, "Cycle through instances")
	flag.BoolVar(&opts.debugAuthors, "debug-authors", true, "Log per-author tweet counts")
	fastCGI := flag.Bool("fastcgi", false, "Use FastCGI instead of listening on -addr")
	format := flag.String("format", "atom", `Feed format to write ("atom", "json", "rss")`)
	instances := flag.String("instances", "https://nitter.net", "Comma-separated list of URLs of Nitter instances to use")
	status := flag.Bool("status", false, "Use Nitter status API to configure instances")
	flag.BoolVar(&opts.rewrite, "rewrite", true, "Rewrite tweet content to point at twitter.com")
	timeout := flag.Int("timeout", 10, "HTTP timeout in seconds for fetching a feed from a Nitter instance")
	user := flag.String("user", "", "User to fetch to stdout (instead of starting a server)")
	flag.Parse()

	opts.format = feedFormat(*format)
	opts.timeout = time.Duration(*timeout) * time.Second
	opts.statusApi = *status

	hnd, err := newHandler(*base, *instances, opts)
	if err != nil {
		log.Fatal("Failed creating handler: ", err)
	}

	if *user != "" {
		w := newFakeResponseWriter()
		req, _ := http.NewRequest(http.MethodGet, "/"+*user, nil)
		hnd.ServeHTTP(w, req)
		if w.status != http.StatusOK {
			log.Fatal(w.msg)
		}
	} else if *fastCGI {
		log.Fatal("Failed serving over FastCGI: ", fcgi.Serve(nil, hnd))
	} else {
		srv := &http.Server{Addr: *addr, Handler: hnd}
		log.Fatalf("Failed serving on %v: %v", *addr, srv.ListenAndServe())
	}
}

// handler implements http.Handler to accept GET requests for RSS feeds.
type handler struct {
	base      *url.URL
	client    http.Client
	instances []*url.URL
	opts      handlerOptions
	start     int        // starting index in instances
	mu        sync.Mutex // protects start
}

type handlerOptions struct {
	cycle        bool // cycle through instances
	timeout      time.Duration
	format       feedFormat
	rewrite      bool // rewrite tweet content to point at Twitter
	debugAuthors bool // log per-author tweet counts
	statusApi    bool
}

type HostStatus struct {
	Url               string
	Domain            string
	Points            int
	Rss               bool
	Recent_pings      []int
	Ping_max          int
	Ping_min          int
	Ping_avg          int
	Version           string
	Version_url       string
	Healthy           bool
	Last_healthy      string
	Is_upstream       bool
	Is_latest_version bool
	Is_bad_host       bool
}

type StatusResponse struct {
	Hosts []HostStatus
}

func fetchStatus(hnd *handler) {
	resp, err := hnd.client.Get("https://status.d420.de/api/v1/instances")
	if err != nil {
		fmt.Println("Couldn't fetch status API")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Println("Bad response code from status API")
		return
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("Can't read status API")
		return
	}

	var statusResponse StatusResponse
	err = json.Unmarshal(body, &statusResponse)
	if err != nil {
		fmt.Println("Can't unmarshal status API \n%s", err)
		return
	}
	for _, h := range statusResponse.Hosts {
		u, uerr := url.Parse(h.Url)
		if uerr != nil {
			fmt.Println("Bad URL", h.Url)
			continue
		}
		if h.Is_bad_host == false && h.Rss == true && h.Healthy == true {
			hnd.instances = append(hnd.instances, u)
			fmt.Println("Added ", h.Url)
		}
	}
}
func newHandler(base, instances string, opts handlerOptions) (*handler, error) {
	hnd := &handler{
		client: http.Client{Timeout: opts.timeout},
		opts:   opts,
	}

	if base != "" {
		var err error
		if hnd.base, err = url.Parse(base); err != nil {
			return nil, fmt.Errorf("failed parsing %q: %v", base, err)
		}
	}

	for _, in := range strings.Split(instances, ",") {
		// Hack to permit trailing commas to make it easier to comment out instances in configs.
		if in == "" {
			continue
		}
		u, err := url.Parse(in)
		if err != nil {
			return nil, fmt.Errorf("failed parsing %q: %v", in, err)
		}
		hnd.instances = append(hnd.instances, u)
	}
	if opts.statusApi {
		fmt.Println("Fetching status API")
		fetchStatus(hnd)
	}
	if len(hnd.instances) == 0 {
		return nil, errors.New("no instances supplied")
	}

	return hnd, nil
}

var (
	// Matches comma-separated Twitter usernames with an optional /media, /search, or /with_replies suffix
	// supported by Nitter's RSS handler (https://github.com/zedeus/nitter/blob/master/src/routes/rss.nim).
	// Ignores any leading junk that might be present in the path e.g. when proxying a prefix to FastCGI.
	userRegexp = regexp.MustCompile(`[_a-zA-Z0-9,]+(/(media|search|with_replies))?$`)

	// Matches valid query parameters to forward to Nitter.
	queryRegexp = regexp.MustCompile(`^max_position=[^&]+$`)
)

func (hnd *handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Only GET supported", http.StatusMethodNotAllowed)
		return
	}

	// Sigh.
	if strings.HasSuffix(req.URL.Path, "favicon.ico") {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	user := userRegexp.FindString(req.URL.Path)
	if user == "" {
		http.Error(w, "Invalid user", http.StatusBadRequest)
		return
	}
	var query string
	if queryRegexp.MatchString(req.URL.RawQuery) {
		query = req.URL.RawQuery
	}

	if hnd.opts.statusApi == true && len(hnd.instances) == 0 {
		fetchStatus(hnd)
		if len(hnd.instances) == 0 {
			http.Error(w, "No instances", http.StatusInternalServerError)
			return
		}
	}

	start := hnd.start
	if hnd.opts.cycle {
		hnd.mu.Lock()
		hnd.start = (hnd.start + 1) % len(hnd.instances)
		hnd.mu.Unlock()
	}

	for i := 0; i < len(hnd.instances); i++ {
		idx := (start + i) % len(hnd.instances)
		in := hnd.instances[idx]
		b, loc, minID, err := hnd.fetch(in, user, query)
		if err != nil {
			log.Printf("Failed fetching %v from %v: %v", user, in, err)
			if hnd.opts.statusApi == true {
				hnd.instances[idx] = hnd.instances[len(hnd.instances)-1]
				hnd.instances = hnd.instances[:len(hnd.instances)-1]
				fmt.Println(hnd.instances)
			}
			continue
		}
		w.Header().Set(minIDHeader, minID)
		if err := hnd.rewrite(w, b, user, loc); err != nil {
			log.Printf("Failed rewriting %v from %v: %v", user, in, err)
			continue
		}
		return
	}
	http.Error(w, "Couldn't get feed from any instances", http.StatusInternalServerError)
}

// fetch fetches user's feed from supplied Nitter instance.
// user follows the format used by Nitter: it can be a single username or a comma-separated
// list of usernames, with an optional /media, /search, or /with_replies suffix.
// If query is non-empty, it will be passed to the instance.
// The response body, final location (after redirects), and Min-Id header value are returned.
func (hnd *handler) fetch(instance *url.URL, user, query string) (
	body []byte, loc *url.URL, minID string, err error) {
	u := *instance
	u.Path = path.Join(u.Path, user, "rss")
	u.RawQuery = query

	log.Print("Fetching ", u.String())
	resp, err := hnd.client.Get(u.String())
	if err != nil {
		return nil, nil, "", err
	}
	defer resp.Body.Close()
	loc = resp.Request.URL
	if resp.StatusCode != http.StatusOK {
		return nil, loc, "", fmt.Errorf("server returned %v (%v)", resp.StatusCode, resp.Status)
	}
	body, err = ioutil.ReadAll(resp.Body)
	return body, loc, resp.Header.Get(minIDHeader), err
}

// rewrite parses user's feed from b and rewrites it to w.
func (hnd *handler) rewrite(w http.ResponseWriter, b []byte, user string, loc *url.URL) error {
	of, err := gofeed.NewParser().ParseString(string(b))
	if err != nil {
		return err
	}

	log.Printf("Rewriting %v item(s) for %v", len(of.Items), user)

	feed := &feeds.Feed{
		Title:       of.Title,
		Link:        &feeds.Link{Href: rewriteTwitterURL(of.Link)},
		Description: "Twitter feed for " + user,
	}
	if of.UpdatedParsed != nil {
		feed.Updated = *of.UpdatedParsed
	}
	if of.Author != nil {
		feed.Author = &feeds.Author{Name: of.Author.Name}
	}

	var img string
	if of.Image != nil {
		img = rewriteIconURL(of.Image.URL)
		feed.Image = &feeds.Image{Url: img}
	}

	authorCnt := make(map[string]int)

	for _, oi := range of.Items {
		// The Content field seems to be empty. gofeed appears to instead return the
		// content (often including HTML) in the Description field.
		content := oi.Description
		if hnd.opts.rewrite {
			if content, err = rewriteContent(oi.Description, loc); err != nil {
				return err
			}
		}

		item := &feeds.Item{
			Title:   oi.Title,
			Link:    &feeds.Link{Href: rewriteTwitterURL(oi.Link)},
			Id:      rewriteTwitterURL(oi.GUID),
			Content: content,
		}

		// When writing a JSON feed, the feeds package seems to expect the Description field to
		// contain text rather than HTML.
		if hnd.opts.format == jsonFormat {
			item.Description = oi.Title
		} else {
			item.Description = content
		}

		if oi.PublishedParsed != nil {
			item.Created = *oi.PublishedParsed
		}
		if oi.UpdatedParsed != nil {
			item.Updated = *oi.UpdatedParsed
		}

		if oi.Author != nil && oi.Author.Name != "" {
			item.Author = &feeds.Author{Name: oi.Author.Name}
		} else if oi.DublinCoreExt != nil && len(oi.DublinCoreExt.Creator) > 0 {
			// Nitter seems to use <dc:creator> for the original author in retweets.
			item.Author = &feeds.Author{Name: oi.DublinCoreExt.Creator[0]}
		}

		authorCnt[item.Author.Name] += 1

		// Nitter dumps the entire content into the title.
		// This looks ugly in Feedly, so truncate it.
		if ut := []rune(item.Title); len(ut) > titleLen {
			item.Title = string(ut[:titleLen-1]) + "…"
		}

		feed.Add(item)
	}

	// I've been seeing an occasional bug where a given feed will suddenly include a bunch of
	// unrelated tweets from some other feed. I'm assuming it's caused by one or more buggy Nitter
	// instances.
	if hnd.opts.debugAuthors {
		log.Printf("Authors for %v: %v", user, authorCnt)
	}

	switch hnd.opts.format {
	case atomFormat:
		af := (&feeds.Atom{Feed: feed}).AtomFeed()
		af.Icon = img
		af.Logo = img
		s, err := feeds.ToXML(af)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/atom+xml; charset=UTF-8")
		_, err = io.WriteString(w, s)
		return err
	case jsonFormat:
		jf := (&feeds.JSON{Feed: feed}).JSONFeed()
		if hnd.base != nil {
			u := *hnd.base
			u.Path = path.Join(u.Path, user)
			jf.FeedUrl = u.String()
		}
		jf.Favicon = img
		jf.Icon = img
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		return enc.Encode(jf)
	case rssFormat:
		w.Header().Set("Content-Type", "application/rss+xml; charset=UTF-8")
		return feed.WriteRss(w)
	default:
		return fmt.Errorf("unknown format %q", hnd.opts.format)
	}
}

const (
	start  = `(?:^|\b)`
	end    = `(?:$|\b)`
	scheme = `https?://`
	host   = `[a-zA-Z0-9][-a-zA-Z0-9]*\.[-.a-zA-Z0-9]+`
	slash  = `(?:/|%2F)` // Nitter seems to incorrectly (?) escape slashes in some cases.
)

// encPicRegexp matches weird Nitter RULs with base64-encoded image paths,
// e.g. "https://example.org/pic/enc/bWVkaWEvRm1Jc0R3SldRQUFKV2w4LmpwZw==".
// We can't use |end| here since \b expects \w on one side and \W on the other,
// but we may have a URL ending with '=' followed by '"' (both \W).
var encPicRegexp = regexp.MustCompile(start +
	// TODO: https://github.com/zedeus/nitter/blob/master/src/utils.nim also has code
	// for /video/enc/ and /pic/orig/enc/. I'm not bothering to decode those yet since
	// there aren't any rewrite patterns to further rewrite the resulting URLs.
	`(` + scheme + host + `/pic/)` + // group 1: start of URL
	`enc/` +
	// See "5. Base 64 Encoding with URL and Filename Safe Alphabet" from RFC 4648.
	`([-_=a-zA-Z0-9]+)`) // group 2: base64-encoded end of URL

// decodeEncPicURL rewrites a URL matched by encPicRegexp to instead be the corresponding
// non-encoded Nitter URL, e.g. "https://example.org/pic/media/FmN39CgWQAEkNAO.jpg".
// If the URL is not matched by encPicRegexp, it will be returned unmodified.
func decodeEncPicURL(u string) string {
	ms := encPicRegexp.FindStringSubmatch(u)
	if ms == nil {
		return u
	}
	dec, err := base64.URLEncoding.DecodeString(ms[2])
	if err != nil {
		log.Printf("Failed base64-decoding %q: %v", ms[2], err)
		return u
	}
	return ms[1] + string(dec)
}

// iconRegexp exactly matches a Nitter profile image URL,
// e.g. "https://example.org/pic/profile_images%2F1234567890%2F_AbQ3eRu_400x400.jpg".
// At some point, Nitter seems to have started adding "/pbs.twimg.com" after "/pic".
var iconRegexp = regexp.MustCompile(`^` +
	scheme + host +
	`/pic(?:/pbs\.twimg\.com)?` + slash +
	`profile_images` + slash +
	`(\d+)` + // group 1: ID
	slash +
	`([-_.a-zA-Z0-9]+)$`) // group 2: ID, size, extension

// rewriteIconURL rewrites a Nitter profile image URL to the corresponding Twitter URL.
func rewriteIconURL(u string) string {
	// First decode base64-encoded /pic/enc paths, which are used by some Nitter instances.
	u = decodeEncPicURL(u)
	ms := iconRegexp.FindStringSubmatch(u)
	if ms == nil {
		return u
	}
	return fmt.Sprintf("https://pbs.twimg.com/profile_images/%v/%v", ms[1], ms[2])
}

// rewritePatterns is used by rewriteContent to rewrite URLs within tweets.
var rewritePatterns = []struct {
	re *regexp.Regexp
	fn func(ms []string) string // matching groups from re are passed
}{
	{
		// Before doing anything else, rewrite base64-encoded image paths.
		// Later rules may rewrite these further.
		encPicRegexp,
		func(ms []string) string { return decodeEncPicURL(ms[0]) },
	},
	{
		// Nitter URL referring to a tweet, e.g.
		// "https://example.org/someuser/status/1234567890#m" or
		// "https://example.org/i/web/status/1234567890".
		// The scheme is optional.
		regexp.MustCompile(start +
			`(` + scheme + `)?` + // group 1: optional scheme
			host + `/` +
			`([_a-zA-Z0-9]+|i/web)` + // group 2: username or weird 'i/web' thing
			slash + `status` + slash +
			`(\d+)` + // group 3: tweet ID
			`(?:#m)?` + // nitter adds these hashes
			end),
		func(ms []string) string {
			u := fmt.Sprintf("twitter.com/%v/status/%v", ms[2], ms[3])
			if ms[1] != "" {
				u = "https://" + u
			}
			return u
		},
	},
	{
		// Nitter URL referring to an image, e.g.
		// "https://example.org/pic/media%2FA3B6MFcQXBBcIa2.jpg".
		regexp.MustCompile(start +
			scheme + host + `/pic` + slash + `media` + slash +
			`([-_a-zA-Z0-9]+)` + // group 1: image ID
			`\.(jpg|png)` + // group 2: extension
			end),
		func(ms []string) string { return fmt.Sprintf("https://pbs.twimg.com/media/%v?format=%v", ms[1], ms[2]) },
	},
	{
		// Nitter URL referring to a video, e.g.
		// "https://example.org/pic/video.twimg.com%2Ftweet_video%2FA47B3e5XMAM233z.mp4".
		regexp.MustCompile(start +
			scheme + host + `/pic` + slash + `video.twimg.com` + slash + `tweet_video` + slash +
			`([-_.a-zA-Z0-9]+)` + // group 1: video name and extension
			end),
		func(ms []string) string { return "https://video.twimg.com/tweet_video/" + ms[1] },
	},
	{
		// Nitter URL referring to a video thumbnail, e.g.
		// "http://example.org/pic/tweet_video_thumb%2FA47B3e5XMAM233z.jpg".
		regexp.MustCompile(start +
			scheme + host + `/pic` + slash + `tweet_video_thumb` + slash +
			`([-_.a-zA-Z0-9]+)` + // group 1: thumbnail name and extension
			end),
		func(ms []string) string { return "https://video.twimg.com/tweet_video_thumb/" + ms[1] },
	},
	{
		// Nitter URL referring to an external (?) video thumbnail, e.g.
		// "https://example.org/pic/ext_tw_video_thumb%2F3516826898992848541%2Fpu%2Fimg%2FaB-5ho5t2AlIL7sK.jpg".
		regexp.MustCompile(start +
			scheme + host + `/pic` + slash + `ext_tw_video_thumb` + slash +
			`(\d+)` + // group 1: tweet ID (?)
			slash + `pu` + slash + `img` + slash +
			`([-_.a-zA-Z0-9]+)` + // group 2: thumbnail name and extension
			end),
		func(ms []string) string {
			return "https://pbs.twimg.com/ext_tw_video_thumb/" + ms[1] + "/pu/img/" + ms[2]
		},
	},
	{
		// Invidious URL referring to a YouTube URL, e.g.
		// "https://example.org/watch?v=AxWGuBDrA1u". The scheme is optional.
		regexp.MustCompile(start +
			`(` + scheme + `)?` + // group 1: optional scheme
			host + `/watch\?v=` +
			`([-_a-zA-Z0-9]+)` + // group 2: video ID
			end),
		func(ms []string) string {
			u := "youtube.com/watch?v=" + ms[2]
			if ms[1] != "" {
				u = "https://" + u
			}
			return u
		},
	},
	{
		// Invidious URL without /watch?v=, e.g.
		// "https://invidious.snopyta.org/AxWGuBDrA1u". The scheme is optional.
		regexp.MustCompile(start +
			`(` + scheme + `)?` + // group 1: optional scheme
			`invidious\.snopyta\.org/` +
			`([-_a-zA-Z0-9]{8,})` + // group 2: video ID
			end),
		func(ms []string) string {
			u := "youtube.com/watch?v=" + ms[2]
			if ms[1] != "" {
				u = "https://" + u
			}
			return u
		},
	},
}

// rewriteContent rewrites a tweet's HTML content fetched from loc.
// Some public Nitter instances seem to be misconfigured, e.g. rewriting URLs to
// start with "http://localhost", so we just modify all URLs that look like they
// can be served by Twitter.
func rewriteContent(s string, loc *url.URL) (string, error) {
	// It'd be better to parse the HTML instead of using regular expressions, but that's quite
	// painful to do (see https://github.com/derat/twittuh) so I'm trying to avoid it for now.
	for _, rw := range rewritePatterns {
		s = rw.re.ReplaceAllStringFunc(s, func(o string) string {
			return rw.fn(rw.re.FindStringSubmatch(o))
		})
	}

	// Match all remaining URLs served by the instance and change them to use twitter.com:
	// https://codeberg.org/derat/nitter-rss-proxy/issues/13
	if loc != nil {
		// Match both http:// and https:// since some instances seem to be configured
		// to always use http:// for links.
		re, err := regexp.Compile(`\bhttps?://` + regexp.QuoteMeta(loc.Host) + `/[^" ]*`)
		if err != nil {
			return s, err
		}
		s = re.ReplaceAllStringFunc(s, func(o string) string { return rewriteTwitterURL(o) })
	}

	// TODO: Fetch embedded tweets.

	// Make sure that newlines are preserved.
	s = strings.ReplaceAll(s, "\n", "<br>")

	return s, nil
}

// rewriteTwitterURL rewrites orig's scheme and hostname to be https://twitter.com.
func rewriteTwitterURL(orig string) string {
	u, err := url.Parse(orig)
	if err != nil {
		log.Printf("Failed parsing %q: %v", orig, err)
		return orig
	}
	u.Scheme = "https"
	u.Host = "twitter.com"
	u.Fragment = "" // get rid of weird '#m' fragments added by Nitter
	return u.String()
}

// fakeResponseWriter is an http.ResponseWriter implementation that just writes to stdout.
// It's used for the -user flag.
type fakeResponseWriter struct {
	status int
	msg    string // error from body if Write is called with non-200 status
}

func newFakeResponseWriter() *fakeResponseWriter {
	// Default to success in case WriteHeader isn't called.
	return &fakeResponseWriter{status: http.StatusOK}
}

func (w *fakeResponseWriter) Header() http.Header { return map[string][]string{} }

func (w *fakeResponseWriter) Write(b []byte) (int, error) {
	if w.status != http.StatusOK {
		w.msg = string(b)
		return len(b), nil
	}
	return os.Stdout.Write(b)
}

func (w *fakeResponseWriter) WriteHeader(statusCode int) { w.status = statusCode }
