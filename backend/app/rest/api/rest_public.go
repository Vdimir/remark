package api

import (
	"bytes"
	"crypto/sha1" // nolint
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/render"
	cache "github.com/go-pkgz/lcw"
	log "github.com/go-pkgz/lgr"
	R "github.com/go-pkgz/rest"
	"github.com/pkg/errors"

	"github.com/umputun/remark42/backend/app/rest"
	"github.com/umputun/remark42/backend/app/store"
	"github.com/umputun/remark42/backend/app/store/image"
	"github.com/umputun/remark42/backend/app/store/service"
)

type public struct {
	dataService      pubStore
	cache            LoadingCache
	readOnlyAge      int
	commentFormatter *store.CommentFormatter
	imageService     *image.Service
	streamer         *Streamer
	webRoot          string
}

type pubStore interface {
	Create(comment store.Comment) (commentID string, err error)
	Get(locator store.Locator, commentID string, user store.User) (store.Comment, error)
	FindSince(locator store.Locator, sort string, user store.User, since time.Time) ([]store.Comment, error)
	Last(siteID string, limit int, since time.Time, user store.User) ([]store.Comment, error)
	User(siteID, userID string, limit, skip int, user store.User) ([]store.Comment, error)
	UserCount(siteID, userID string) (int, error)
	Count(locator store.Locator) (int, error)
	List(siteID string, limit int, skip int) ([]store.PostInfo, error)
	Info(locator store.Locator, readonlyAge int) (store.PostInfo, error)
	Search(siteID, query, sortBy string, from, limit int) (*service.SearchResultPage, error)
	SearchHelp() (service.SearchHelpPrompt, error)

	ValidateComment(c *store.Comment) error
	IsReadOnly(locator store.Locator) bool
	Counts(siteID string, postIDs []string) ([]store.PostInfo, error)
}

// GET /find?site=siteID&url=post-url&format=[tree|plain]&sort=[+/-time|+/-score|+/-controversy]&view=[user|all]&since=unix_ts_msec
// find comments for given post. Returns in tree or plain formats, sorted
func (s *public) findCommentsCtrl(w http.ResponseWriter, r *http.Request) {
	locator := store.Locator{SiteID: r.URL.Query().Get("site"), URL: r.URL.Query().Get("url")}
	sort := r.URL.Query().Get("sort")
	if strings.HasPrefix(sort, " ") { // restore + replaced by " "
		sort = "+" + sort[1:]
	}

	view := r.URL.Query().Get("view")
	since, err := s.parseSince(r)
	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't parse since", rest.ErrCommentNotFound)
		return
	}
	format := r.URL.Query().Get("format")
	if format == "tree" {
		since = time.Time{} // since doesn't make sense for tree
	}

	log.Printf("[DEBUG] get comments for %+v, sort %s, format %s, since %v", locator, sort, format, since)

	key := cache.NewKey(locator.SiteID).ID(URLKeyWithUser(r)).Scopes(locator.SiteID, locator.URL)
	data, err := s.cache.Get(key, func() ([]byte, error) {
		comments, e := s.dataService.FindSince(locator, sort, rest.GetUserOrEmpty(r), since)
		if e != nil {
			comments = []store.Comment{} // error should clear comments and continue for post info
		}
		comments = s.applyView(comments, view)
		var b []byte
		switch format {
		case "tree":
			tree := service.MakeTree(comments, sort, s.readOnlyAge)
			if tree.Nodes == nil { // eliminate json nil serialization
				tree.Nodes = []*service.Node{}
			}
			if s.dataService.IsReadOnly(locator) {
				tree.Info.ReadOnly = true
			}
			b, e = encodeJSONWithHTML(tree)
		default:
			withInfo := commentsWithInfo{Comments: comments}
			if info, ee := s.dataService.Info(locator, s.readOnlyAge); ee == nil {
				withInfo.Info = info
			}
			b, e = encodeJSONWithHTML(withInfo)
		}
		return b, e
	})

	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't find comments", rest.ErrCommentNotFound)
		return
	}

	if err = R.RenderJSONFromBytes(w, r, data); err != nil {
		log.Printf("[WARN] can't render comments for post %+v", locator)
	}
}

// POST /preview, body is a comment, returns rendered html
func (s *public) previewCommentCtrl(w http.ResponseWriter, r *http.Request) {
	comment := store.Comment{}
	if err := render.DecodeJSON(http.MaxBytesReader(w, r.Body, hardBodyLimit), &comment); err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't bind comment", rest.ErrDecode)
		return
	}

	user, err := rest.GetUserInfo(r)
	if err != nil { // this not suppose to happen (handled by Auth), just dbl-check
		rest.SendErrorJSON(w, r, http.StatusUnauthorized, err, "can't get user info", rest.ErrNoAccess)
		return
	}
	comment.User = user
	comment.Orig = comment.Text
	if err = s.dataService.ValidateComment(&comment); err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "invalid comment", rest.ErrCommentValidation)
		return
	}

	comment = s.commentFormatter.Format(comment)
	comment.Sanitize()
	render.HTML(w, r, comment.Text)
}

// GET /info?site=siteID&url=post-url - get info about the post
func (s *public) infoCtrl(w http.ResponseWriter, r *http.Request) {
	locator := store.Locator{SiteID: r.URL.Query().Get("site"), URL: r.URL.Query().Get("url")}

	key := cache.NewKey(locator.SiteID).ID(URLKey(r)).Scopes(locator.SiteID, locator.URL)
	data, err := s.cache.Get(key, func() ([]byte, error) {
		info, e := s.dataService.Info(locator, s.readOnlyAge)
		if e != nil {
			return nil, e
		}
		return encodeJSONWithHTML(info)
	})

	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't get post info", rest.ErrPostNotFound)
		return
	}

	if err = R.RenderJSONFromBytes(w, r, data); err != nil {
		log.Printf("[WARN] can't render info for post %+v", locator)
	}
}

// GET /stream/info?site=siteID&url=post-url&since=unix_ts_msec - get info stream about the post
func (s *public) infoStreamCtrl(w http.ResponseWriter, r *http.Request) {
	locator := store.Locator{SiteID: r.URL.Query().Get("site"), URL: r.URL.Query().Get("url")}
	log.Printf("[DEBUG] start stream for %+v, timeout=%v, refresh=%v", locator, s.streamer.TimeOut, s.streamer.Refresh)

	sinceTS, err := s.parseSince(r)
	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't translate since parameter", rest.ErrDecode)
		return
	}

	fn := func() steamEventFn {
		lastTS := sinceTS
		lastCount := 0

		return func() (event string, data []byte, upd bool, err error) {
			key := cache.NewKey(locator.SiteID).ID(URLKey(r)).Scopes(locator.SiteID, locator.URL)
			data, err = s.cache.Get(key, func() ([]byte, error) {
				info, e := s.dataService.Info(locator, s.readOnlyAge)
				if e != nil {
					return nil, e
				}
				// cache update used as indication of post update. comparing lastTS for no-cache.
				// removal won't update lastTS, count check will catch it.
				if !lastTS.IsZero() && (info.LastTS != lastTS || info.Count != lastCount) {
					upd = true
				}
				lastTS = info.LastTS
				lastCount = info.Count
				return encodeJSONWithHTML(info)
			})
			if err != nil {
				return "info", data, false, err
			}
			return "info", data, upd, nil
		}
	}

	if e := s.streamer.Activate(r.Context(), fn, w); e != nil {
		rest.SendErrorJSON(w, r, http.StatusInternalServerError, e, "can't stream", rest.ErrInternal)
	}
}

// GET /last/{limit}?site=siteID&since=unix_ts_msec - last comments for the siteID, across all posts, sorted by time, optionally
// limited with "since" param
func (s *public) lastCommentsCtrl(w http.ResponseWriter, r *http.Request) {
	siteID := r.URL.Query().Get("site")
	log.Printf("[DEBUG] get last comments for %s", siteID)

	limit, err := strconv.Atoi(chi.URLParam(r, "limit"))
	if err != nil {
		limit = 0
	}

	sinceTime, err := s.parseSince(r)
	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't translate since parameter", rest.ErrDecode)
		return
	}

	key := cache.NewKey(siteID).ID(URLKey(r)).Scopes(lastCommentsScope)
	data, err := s.cache.Get(key, func() ([]byte, error) {
		comments, e := s.dataService.Last(siteID, limit, sinceTime, rest.GetUserOrEmpty(r))
		if e != nil {
			return nil, e
		}
		// filter deleted from last comments view. Blocked marked as deleted and will sneak in without
		filterDeleted := filterComments(comments, func(c store.Comment) bool { return !c.Deleted })
		return encodeJSONWithHTML(filterDeleted)
	})

	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusInternalServerError, err, "can't get last comments", rest.ErrInternal)
		return
	}

	if err = R.RenderJSONFromBytes(w, r, data); err != nil {
		log.Printf("[WARN] can't render last comments for site %s", siteID)
	}
}

// GET /stream/last?site=siteID&since=unix_ts_ms - stream of last comments last comments for the siteID, across all posts
func (s *public) lastCommentsStreamCtrl(w http.ResponseWriter, r *http.Request) {
	siteID := r.URL.Query().Get("site")
	log.Printf("[DEBUG] get last comments stream for %s", siteID)

	sinceTS, err := s.parseSince(r)
	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't translate since parameter", rest.ErrDecode)
		return
	}
	if sinceTS.IsZero() {
		sinceTS = time.Now()
	}

	fn := func() steamEventFn {
		sinceTime := sinceTS
		return func() (event string, data []byte, upd bool, err error) {
			key := cache.NewKey(siteID).ID(URLKey(r)).Scopes(lastCommentsScope)
			data, err = s.cache.Get(key, func() ([]byte, error) {
				comments, e := s.dataService.Last(siteID, 1, sinceTime, rest.GetUserOrEmpty(r))
				if e != nil {
					return nil, e
				}
				sinceTime = time.Now()
				if len(comments) > 0 {
					sinceTime = comments[0].Timestamp
					upd = true
				}
				return encodeJSONWithHTML(comments)
			})
			return "last", data, upd, err
		}
	}

	if e := s.streamer.Activate(r.Context(), fn, w); e != nil {
		rest.SendErrorJSON(w, r, http.StatusInternalServerError, e, "can't stream", rest.ErrInternal)
	}
}

// GET /id/{id}?site=siteID&url=post-url - gets a comment by id
func (s *public) commentByIDCtrl(w http.ResponseWriter, r *http.Request) {

	id := chi.URLParam(r, "id")
	siteID := r.URL.Query().Get("site")
	url := r.URL.Query().Get("url")

	log.Printf("[DEBUG] get comments by id %s, %s %s", id, siteID, url)

	comment, err := s.dataService.Get(store.Locator{SiteID: siteID, URL: url}, id, rest.GetUserOrEmpty(r))
	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't get comment by id", rest.ErrCommentNotFound)
		return
	}
	render.Status(r, http.StatusOK)

	if err = R.RenderJSONWithHTML(w, r, comment); err != nil {
		log.Printf("[WARN] can't render last comments for url=%s, id=%s", url, id)
	}
}

// GET /comments?site=siteID&user=id - returns comments for given userID
func (s *public) findUserCommentsCtrl(w http.ResponseWriter, r *http.Request) {

	userID := r.URL.Query().Get("user")
	siteID := r.URL.Query().Get("site")

	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil {
		limit = 0
	}

	resp := struct {
		Comments []store.Comment `json:"comments,omitempty"`
		Count    int             `json:"count,omitempty"`
	}{}

	log.Printf("[DEBUG] get comments for userID %s, %s", userID, siteID)

	key := cache.NewKey(siteID).ID(URLKeyWithUser(r)).Scopes(userID, siteID)
	data, err := s.cache.Get(key, func() ([]byte, error) {
		comments, e := s.dataService.User(siteID, userID, limit, 0, rest.GetUserOrEmpty(r))
		if e != nil {
			return nil, e
		}
		comments = filterComments(comments, func(c store.Comment) bool { return !c.Deleted })
		count, e := s.dataService.UserCount(siteID, userID)
		if e != nil {
			return nil, e
		}
		resp.Comments, resp.Count = comments, count
		return encodeJSONWithHTML(resp)
	})

	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't get comment by user id", rest.ErrCommentNotFound)
		return
	}

	if err = R.RenderJSONFromBytes(w, r, data); err != nil {
		log.Printf("[WARN] can't render found comments for user %s", userID)
	}
}

// GET /count?site=siteID&url=post-url - get number of comments for given post
func (s *public) countCtrl(w http.ResponseWriter, r *http.Request) {
	locator := store.Locator{SiteID: r.URL.Query().Get("site"), URL: r.URL.Query().Get("url")}
	count, err := s.dataService.Count(locator)
	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't get count", rest.ErrPostNotFound)
		return
	}
	render.JSON(w, r, R.JSON{"count": count, "locator": locator})
}

// POST /counts?site=siteID - get number of comments for posts from post body
func (s *public) countMultiCtrl(w http.ResponseWriter, r *http.Request) {
	const countBodyLimit int64 = 1024 * 128 // count request can be big for some site because it lists all urls
	siteID := r.URL.Query().Get("site")
	posts := []string{}
	if err := render.DecodeJSON(http.MaxBytesReader(w, r.Body, countBodyLimit), &posts); err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't get list of posts from request", rest.ErrSiteNotFound)
		return
	}

	// key could be long for multiple posts, make it sha1
	k := URLKey(r) + strings.Join(posts, ",")
	h := sha1.Sum([]byte(k)) // nolint
	sha := base64.URLEncoding.EncodeToString(h[:])

	key := cache.NewKey(siteID).ID(sha).Scopes(siteID)
	data, err := s.cache.Get(key, func() ([]byte, error) {
		counts, e := s.dataService.Counts(siteID, posts)
		if e != nil {
			return nil, e
		}
		return encodeJSONWithHTML(counts)
	})

	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't get counts for "+siteID, rest.ErrSiteNotFound)
		return
	}

	if err = R.RenderJSONFromBytes(w, r, data); err != nil {
		log.Printf("[WARN] can't render comments counters site %s", siteID)
	}
}

// GET /list?site=siteID&limit=50&skip=10 - list posts with comments
func (s *public) listCtrl(w http.ResponseWriter, r *http.Request) {

	siteID := r.URL.Query().Get("site")
	limit, skip := 0, 0

	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		limit = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("skip")); err == nil {
		skip = v
	}

	key := cache.NewKey(siteID).ID(URLKey(r)).Scopes(siteID)
	data, err := s.cache.Get(key, func() ([]byte, error) {
		posts, e := s.dataService.List(siteID, limit, skip)
		if e != nil {
			return nil, e
		}
		return encodeJSONWithHTML(posts)
	})

	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't get list of comments for "+siteID, rest.ErrSiteNotFound)
		return
	}

	if err = R.RenderJSONFromBytes(w, r, data); err != nil {
		log.Printf("[WARN] can't render posts list for site %s", siteID)
	}
}

// GET /picture/{user}/{id} - get picture
func (s *public) loadPictureCtrl(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "user") + "/" + chi.URLParam(r, "id")
	img, err := s.imageService.Load(id)
	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't get image "+id, rest.ErrAssetNotFound)
		return
	}
	// enforce client-side caching
	etag := `"` + id + `"`
	w.Header().Set("Etag", etag)
	w.Header().Set("Cache-Control", "max-age=604800") // 7 days
	if match := r.Header.Get("If-None-Match"); match != "" {
		if strings.Contains(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	w.Header().Set("Content-Type", s.imageService.ImgContentType(img))
	w.Header().Set("Content-Length", strconv.Itoa(len(img)))
	w.WriteHeader(http.StatusOK)
	if _, err = io.Copy(w, bytes.NewReader(img)); err != nil {
		log.Printf("[WARN] can't send response to %s, %s", r.RemoteAddr, err)
	}
}

// GET /search?site=siteID&query=queryText&limit=20&skip=10 - search documents
func (s *public) searchQueryCtrl(w http.ResponseWriter, r *http.Request) {
	maxSearchLimit := 100
	siteID := r.URL.Query().Get("site")
	query := r.URL.Query().Get("query")
	sortBy := r.URL.Query().Get("sort")

	limit, skip := 20, 0

	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		limit = v
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("skip")); err == nil {
		skip = v
	}

	if limit < 1 || maxSearchLimit < limit {
		rest.SendErrorJSON(w, r,
			http.StatusBadRequest,
			errors.Errorf("wrong param"),
			fmt.Sprintf("wrong limit value. expected to be from 1 to %d", maxSearchLimit),
			rest.ErrActionRejected)
		return
	}

	key := cache.NewKey(query).ID(URLKey(r)).Scopes(siteID, searchScope)
	data, err := s.cache.Get(key, func() ([]byte, error) {
		comments, searchErr := s.dataService.Search(siteID, query, sortBy, skip, limit)
		if searchErr != nil {
			return nil, searchErr
		}

		return encodeJSONWithHTML(comments)
	})

	if errors.Is(err, service.ErrSearchNotReady) {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't perform search request", rest.ErrActionRejected)
		return
	} else if errors.Is(err, service.ErrSearchNotEnabled) {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't perform search request", rest.ErrActionRejected)
		return
	} else if err != nil {
		rest.SendErrorJSON(w, r, http.StatusInternalServerError, err, "can't perform search request", rest.ErrInternal)
		return
	}

	if err = R.RenderJSONFromBytes(w, r, data); err != nil {
		log.Printf("[WARN] can't render search results for site %s", siteID)
	}
}

// GET /search-help - help for search query language
func (s *public) searchHelpCtrl(w http.ResponseWriter, r *http.Request) {
	prompt, err := s.dataService.SearchHelp()
	if errors.Is(err, service.ErrSearchNotEnabled) {
		rest.SendErrorJSON(w, r, http.StatusBadRequest, err, "can't get search help text", rest.ErrActionRejected)
		return
	} else if err != nil {
		rest.SendErrorJSON(w, r, http.StatusInternalServerError, err, "can't get search help text", rest.ErrInternal)
		return
	}

	data, err := encodeJSONWithHTML(prompt)

	if err != nil {
		rest.SendErrorJSON(w, r, http.StatusInternalServerError, err, "can't perform search request", rest.ErrInternal)
		return
	}

	if err = R.RenderJSONFromBytes(w, r, data); err != nil {
		log.Printf("[WARN] can't render search prompt for site")
	}
}

// GET /index.html - respond to /index.html with the content of getstarted.html under /web root
func (s *public) getStartedCtrl(w http.ResponseWriter, r *http.Request) {
	data, err := ioutil.ReadFile(path.Join(s.webRoot, "getstarted.html"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	render.HTML(w, r, string(data))
}

// GET /robots.txt
func (s *public) robotsCtrl(w http.ResponseWriter, r *http.Request) {
	allowed := []string{"/find", "/last", "/id", "/count", "/counts", "/list", "/config", "/user",
		"/img", "/avatar", "/picture"}
	for i := range allowed {
		allowed[i] = "Allow: /api/v1" + allowed[i]
	}
	render.PlainText(w, r, "User-agent: *\nDisallow: /auth/\nDisallow: /api/\n"+strings.Join(allowed, "\n")+"\n")
}

func (s *public) applyView(comments []store.Comment, view string) []store.Comment {
	if strings.EqualFold(view, "user") {
		projection := make([]store.Comment, len(comments))
		for i, c := range comments {
			p := store.Comment{
				ID:   c.ID,
				User: c.User,
			}
			projection[i] = p
		}
		return projection
	}
	return comments
}

func (s *public) parseSince(r *http.Request) (time.Time, error) {
	sinceTS := time.Time{}
	if since := r.URL.Query().Get("since"); since != "" {
		unixTS, e := strconv.ParseInt(since, 10, 64)
		if e != nil {
			return time.Time{}, errors.Wrap(e, "can't translate since parameter")
		}
		sinceTS = time.Unix(unixTS/1000, 1000000*(unixTS%1000)) // since param in msec timestamp
	}
	return sinceTS, nil
}
