package main

import (
	"bytes"
	_ "code.google.com/p/gorilla/mux"
	"code.google.com/p/gorilla/securecookie"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/garyburd/go-oauth/oauth"
	"html/template"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	_ "net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var (
	configPath   = flag.String("config", "config.json", "Path to configuration file")
	httpAddr     = flag.String("addr", ":5020", "HTTP server address")
	inProduction = flag.Bool("production", false, "are we running in production")
	cookieName   = "ckie"
)

var (
	oauthClient = oauth.Client{
		TemporaryCredentialRequestURI: "https://api.twitter.com/oauth/request_token",
		ResourceOwnerAuthorizationURI: "https://api.twitter.com/oauth/authenticate",
		TokenRequestURI:               "https://api.twitter.com/oauth/access_token",
	}

	config = struct {
		TwitterOAuthCredentials *oauth.Credentials
		CookieAuthKeyHexStr     *string
		CookieEncrKeyHexStr     *string
		AnalyticsCode           *string
		AwsAccess               *string
		AwsSecret               *string
		S3BackupBucket          *string
		S3BackupDir             *string
	}{
		&oauthClient.Credentials,
		nil, nil,
		nil,
		nil, nil,
		nil, nil,
	}
	logger        *ServerLogger
	cookieAuthKey []byte
	cookieEncrKey []byte
	secureCookie  *securecookie.SecureCookie

	dataDir string

	tmplLogs                 = "logs.html"
	tmplTimings              = "timings.html"
	tmplMainPage             = "mainpage.html"
	tmplArticle              = "article.html"
	tmplArchive              = "archive.html"
	tmplEdit                 = "edit.html"
	tmplCrashReportsIndex    = "crash_reports_index.html"
	tmplCrashReportsAppIndex = "crash_reports_app_index.html"
	tmplCrashReport          = "crash_report.html"
	templateNames            = [...]string{tmplLogs, tmplMainPage, tmplArticle,
		tmplArchive, tmplEdit, tmplCrashReportsIndex, tmplCrashReportsAppIndex,
		tmplCrashReport, tmplTimings,
		"analytics.html", "inline_css.html", "tagcloud.js", "page_navbar.html"}
	templatePaths []string
	templates     *template.Template

	store           *Store
	storeCrashes    *StoreCrashes
	reloadTemplates = true
	alwaysLogTime   = true
)

func StringEmpty(s *string) bool {
	return s == nil || 0 == len(*s)
}

func S3BackupEnabled() bool {
	if !*inProduction {
		logger.Notice("s3 backups disabled because not in production")
		return false
	}
	if StringEmpty(config.AwsAccess) {
		logger.Notice("s3 backups disabled because AwsAccess not defined in config.json\n")
		return false
	}
	if StringEmpty(config.AwsSecret) {
		logger.Notice("s3 backups disabled because AwsSecret not defined in config.json\n")
		return false
	}
	if StringEmpty(config.S3BackupBucket) {
		logger.Notice("s3 backups disabled because S3BackupBucket not defined in config.json\n")
		return false
	}
	if StringEmpty(config.S3BackupDir) {
		logger.Notice("s3 backups disabled because S3BackupDir not defined in config.json\n")
		return false
	}
	return true
}

func getDataDir() string {
	if dataDir != "" {
		return dataDir
	}
	// locally
	dataDir = filepath.Join("..", "..", "blogdata")
	if PathExists(dataDir) {
		return dataDir
	}
	// on the server
	dataDir = filepath.Join("..", "..", "data")
	if PathExists(dataDir) {
		return dataDir
	}
	log.Fatal("data directory (../../data or ../../blogdata) doesn't exist")
	return ""
}

func GetTemplates() *template.Template {
	if reloadTemplates || (nil == templates) {
		if 0 == len(templatePaths) {
			for _, name := range templateNames {
				templatePaths = append(templatePaths, filepath.Join("tmpl", name))
			}
		}
		templates = template.Must(template.ParseFiles(templatePaths...))
	}
	return templates
}

func ExecTemplate(w http.ResponseWriter, templateName string, model interface{}) bool {
	var buf bytes.Buffer
	if err := GetTemplates().ExecuteTemplate(&buf, templateName, model); err != nil {
		logger.Errorf("Failed to execute template '%s', error: %s", templateName, err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return false
	} else {
		// at this point we ignore error
		w.Header().Set("Content-Length", strconv.Itoa(len(buf.Bytes())))
		w.Write(buf.Bytes())
	}
	return true
}

func isTopLevelUrl(url string) bool {
	return 0 == len(url) || "/" == url
}

func getReferer(r *http.Request) string {
	return r.Header.Get("Referer")
}

// this list was determined by watching /logs
var noLog404 = map[string]bool{
	"/crossdomain.xml":                                               true,
	"/article/Exercise-links-1.html":                                 true,
	"/article/Ecco-for-free.html":                                    true,
	"/article/Disappointed-by-The-Bat.html":                          true,
	"/article/Comments-need-not-apply.html":                          true,
	"/article/Browsing-Newton.html":                                  true,
	"/article/Perl-and-lisp-programmers.html":                        true,
	"/article/iPod-competition.html":                                 true,
	"/article/Programming-Jabber.html":                               true,
	"/article/Good-software-design-contradicts-eXtreme-Program.html": true,
	"/article/Bloglines-vs-Google-Reader-the-verdict.html":           true,
	"/2002/07/30/stuid-coding-mistake-of-the-day.html":               true,
	"/article/Corman-Lisp.html":                                      true,
	"/article/Offshore-outsourcing.html":                             true,
	"/article/Nabble-hosted-forums.html":                             true,
}

func shouldLog404(s string) bool {
	if strings.HasPrefix(s, "/apple-touch-icon") {
		return false
	}
	_, ok := noLog404[s]
	return !ok
}

func serve404(w http.ResponseWriter, r *http.Request) {
	if getReferer(r) != "" && shouldLog404(r.URL.Path) {
		logger.Noticef("404: '%s', referer: '%s'", r.URL.Path, getReferer(r))
	}
	http.NotFound(w, r)
}

func serveErrorMsg(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusBadRequest)
}

func userIsAdmin(cookie *SecureCookieValue) bool {
	return cookie.TwitterUser == "kjk"
}

// reads the configuration file from the path specified by
// the config command line flag.
func readConfig(configFile string) error {
	b, err := ioutil.ReadFile(configFile)
	if err != nil {
		return err
	}
	err = json.Unmarshal(b, &config)
	if err != nil {
		return err
	}
	cookieAuthKey, err = hex.DecodeString(*config.CookieAuthKeyHexStr)
	if err != nil {
		return err
	}
	cookieEncrKey, err = hex.DecodeString(*config.CookieEncrKeyHexStr)
	if err != nil {
		return err
	}
	secureCookie = securecookie.New(cookieAuthKey, cookieEncrKey)
	// verify auth/encr keys are correct
	val := map[string]string{
		"foo": "bar",
	}
	_, err = secureCookie.Encode(cookieName, val)
	if err != nil {
		// for convenience, if the auth/encr keys are not set,
		// generate valid, random value for them
		auth := securecookie.GenerateRandomKey(32)
		encr := securecookie.GenerateRandomKey(32)
		fmt.Printf("auth: %s\nencr: %s\n", hex.EncodeToString(auth), hex.EncodeToString(encr))
	}
	// TODO: somehow verify twitter creds
	return err
}

// Request.RemoteAddress contains port, which we want to remove i.e.:
// "[::1]:58292" => "[::1]"
func ipAddrFromRemoteAddr(s string) string {
	idx := strings.LastIndex(s, ":")
	if idx == -1 {
		return s
	}
	return s[:idx]
}

func getIpAddress(r *http.Request) string {
	hdr := r.Header
	hdrRealIp := hdr.Get("X-Real-Ip")
	hdrForwardedFor := hdr.Get("X-Forwarded-For")
	if hdrRealIp == "" && hdrForwardedFor == "" {
		return ipAddrFromRemoteAddr(r.RemoteAddr)
	}
	if hdrForwardedFor != "" {
		// X-Forwarded-For is potentially a list of addresses separated with ","
		parts := strings.Split(hdrForwardedFor, ",")
		for i, p := range parts {
			parts[i] = strings.TrimSpace(p)
		}
		// TODO: should return first non-local address
		return parts[0]
	}
	return hdrRealIp
}

func jQueryUrl() string {
	//return "//ajax.googleapis.com/ajax/libs/jquery/1.8.2/jquery.min.js"
	//return "/js/jquery-1.4.2.js"
	return "//cdnjs.cloudflare.com/ajax/libs/jquery/1.8.3/jquery.min.js"
}

func prettifyJsUrl() string {
	//return "//cdnjs.cloudflare.com/ajax/libs/prettify/188.0.0/prettify.js"
	//return "/js/prettify.js"
	return "//cdnjs.cloudflare.com/ajax/libs/prettify/r224/prettify.js"
}

func prettifyCssUrl() string {
	//return "/js/prettify.css"
	return "//cdnjs.cloudflare.com/ajax/libs/prettify/r224/prettify.css"
}

func makeTimingHandler(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {

		startTime := time.Now()
		fn(w, r)
		duration := time.Now().Sub(startTime)
		// log urls that take long time to generate i.e. over 1 sec in production
		// or over 0.1 sec in dev
		shouldLog := duration.Seconds() > 1.0
		if alwaysLogTime && duration.Seconds() > 0.1 {
			shouldLog = true
		}
		if shouldLog {
			url := r.URL.Path
			if len(r.URL.RawQuery) > 0 {
				url = fmt.Sprintf("%s?%s", url, r.URL.RawQuery)
			}
			logger.Noticef("'%s' took %f seconds to serve", url, duration.Seconds())
		}
		// TODO: add query to url
		LogSlowPage(r.URL.Path, duration)
	}
}

var emptyString = ""

var test = []byte(`Crashed thread:
0114C072 01:0004B072 sumatrapdf.exe!CrashMe+0x2 c:\users\kkowalczyk\src\sumatrapdf\src\utils\baseutil.cpp+14
0112F0AD 01:0002E0AD sumatrapdf.exe!PrintToDevice+0x1d c:\users\kkowalczyk\src\sumatrapdf\src\print.cpp+111
011303E2 01:0002F3E2 sumatrapdf.exe!PrintThreadData::PrintThread+0x42 c:\users\kkowalczyk\src\sumatrapdf\src\print.cpp+420
76031114 01:00050114 kernel32.dll!BaseThreadInitThunk+0x12
7757B299 01:0005A299 ntdll.dll!RtlInitializeExceptionChain+0x63
7757B26C 01:0005A26C ntdll.dll!RtlInitializeExceptionChain+0x36`)

func main() {
	var err error

	runtime.GOMAXPROCS(runtime.NumCPU())
	flag.Parse()

	/*findFileFixes("../../../sumatrapdf")
	return
	s := linkifyCrashReport(test)
	fmt.Print(string(s))
	return*/

	if *inProduction {
		reloadTemplates = false
		alwaysLogTime = false
	}

	useStdout := !*inProduction
	logger = NewServerLogger(256, 256, useStdout)

	rand.Seed(time.Now().UnixNano())

	if err := readConfig(*configPath); err != nil {
		log.Fatalf("Failed reading config file %s. %s\n", *configPath, err.Error())
	}

	if !*inProduction {
		config.AnalyticsCode = &emptyString
	}

	if store, err = NewStore(getDataDir()); err != nil {
		log.Fatalf("NewStore() failed with %s", err.Error())
	}

	if storeCrashes, err = NewStoreCrashes(getDataDir()); err != nil {
		log.Fatalf("NewStoreCrashes() failed with %s", err.Error())
	}

	readRedirects()

	http.Handle("/", makeTimingHandler(handleMainPage))
	http.HandleFunc("/favicon.ico", handleFavicon)
	http.HandleFunc("/robots.txt", handleRobotsTxt)
	http.HandleFunc("/logs", handleLogs)
	http.HandleFunc("/timings", handleTimings)
	http.HandleFunc("/oauthtwittercb", handleOauthTwitterCallback)
	http.HandleFunc("/login", handleLogin)
	http.HandleFunc("/logout", handleLogout)

	http.Handle("/app/edit", makeTimingHandler(handleAppEdit))
	http.Handle("/app/preview", makeTimingHandler(handleAppPreview))
	http.Handle("/app/delete", makeTimingHandler(handleAppDelete))
	http.Handle("/app/undelete", makeTimingHandler(handleAppUndelete))
	http.Handle("/app/showdeleted", makeTimingHandler(handleAppShowDeleted))
	http.Handle("/app/showprivate", makeTimingHandler(handleAppShowPrivate))
	http.Handle("/app/crashsubmit", makeTimingHandler(handleCrashSubmit))
	http.Handle("/app/crashes", makeTimingHandler(handleCrashes))
	http.Handle("/app/crashesrss", makeTimingHandler(handleCrashesRss))
	http.Handle("/app/crashshow", makeTimingHandler(handleCrashShow))
	http.Handle("/feedburner.xml", makeTimingHandler(handleFeedburnerAtom))
	http.Handle("/atom-all.xml", makeTimingHandler(handleAtomAll))
	http.Handle("/archives.html", makeTimingHandler(handleArchives))
	http.Handle("/software", makeTimingHandler(handleSoftware))
	http.Handle("/software/", makeTimingHandler(handleSoftware))
	http.Handle("/extremeoptimizations/", makeTimingHandler(handleExtremeOpt))
	http.Handle("/article/", makeTimingHandler(handleArticle))
	http.Handle("/kb/", makeTimingHandler(handleArticle))
	http.Handle("/blog/", makeTimingHandler(handleArticle))
	http.Handle("/forum_sumatra/", makeTimingHandler(forumRedirect))
	http.Handle("/articles/", makeTimingHandler(handleArticles))
	http.Handle("/tag/", makeTimingHandler(handleTag))
	http.Handle("/static/", makeTimingHandler(handleStatic))
	http.Handle("/css/", makeTimingHandler(handleCss))
	http.Handle("/js/", makeTimingHandler(handleJs))
	http.Handle("/gfx/", makeTimingHandler(handleGfx))
	http.Handle("/markitup/", makeTimingHandler(handleMarkitup))
	http.Handle("/djs/", makeTimingHandler(handleDjs))
	//http.HandleFunc("/blog", handleBlogMain)

	backupConfig := &BackupConfig{
		AwsAccess: *config.AwsAccess,
		AwsSecret: *config.AwsSecret,
		Bucket:    *config.S3BackupBucket,
		S3Dir:     *config.S3BackupDir,
		LocalDir:  getDataDir(),
	}

	if S3BackupEnabled() {
		go BackupLoop(backupConfig)
	}

	logger.Noticef(fmt.Sprintf("Started runing on %s", *httpAddr))
	if err := http.ListenAndServe(*httpAddr, nil); err != nil {
		fmt.Printf("http.ListendAndServer() failed with %s\n", err.Error())
	}
	fmt.Printf("Exited\n")
}
