package api_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
)

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

type cookieJar struct {
	mu      sync.Mutex
	cookies []*http.Cookie
}

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.cookies = cs
}

func (j *cookieJar) Cookies(_ *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cookies
}
