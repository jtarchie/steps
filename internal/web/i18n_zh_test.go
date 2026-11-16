package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSimplifiedChineseIsServedToBrowsersThatAskForIt(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	for _, ask := range []string{"zh-CN", "zh", "zh-TW", "zh-HK", "fr, zh-CN;q=0.5"} {
		_, body := getAs(t, server, "/p/demo", http.Header{"Accept-Language": {ask}})

		if !strings.Contains(body, `<html lang="zh-CN">`) || !strings.Contains(body, ">运行</a>") {
			t.Errorf("Accept-Language %q: not served in Simplified Chinese", ask)
		}
	}

	_, body := getAs(t, server, "/p/demo", http.Header{"Accept-Language": {"en-US,zh;q=0.3"}})
	if !strings.Contains(body, `<html lang="en">`) {
		t.Error("an English browser was not served English")
	}
}

func TestThePickerOverridesTheBrowserAndRemembersIt(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	pick := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/lang?"+query, nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		return rec
	}

	rec := pick("lang=zh-CN&return=" + url.QueryEscape("/p/demo/runs"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/p/demo/runs" {
		t.Fatalf("GET /lang = %d to %q", rec.Code, rec.Header().Get("Location"))
	}

	cookie := rec.Result().Cookies()[0]
	if cookie.Name != langCookie || cookie.Value != "zh-CN" || !cookie.HttpOnly || cookie.MaxAge <= 0 {
		t.Fatalf("cookie = %+v", cookie)
	}

	_, body := getAs(t, server, "/p/demo", http.Header{"Accept-Language": {"en"}, "Cookie": {cookie.String()}})
	if !strings.Contains(body, `<html lang="zh-CN">`) || !strings.Contains(body, `<a lang="zh-CN" aria-current="true"`) {
		t.Error("the choice did not beat the browser, or the picker does not show it")
	}
}

func TestThePickerOffersEveryLanguageByItsOwnName(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := getAs(t, server, "/p/demo", nil)
	if !strings.Contains(body, `<a aria-current="true" href="/lang?lang=&amp;return=`) || !strings.Contains(body, `>简体中文</a>`) {
		t.Error("with no choice the picker should read auto and offer every language by its own name")
	}
}

func TestThePickerRefusesWhatItDoesNotSpeakOrFollow(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	pick := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/lang?"+query, nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		return rec
	}

	if got := pick("lang=zh-CN&return=" + url.QueryEscape("//evil.example")).Header().Get("Location"); got != "/" {
		t.Errorf("an off-site return was followed: %q", got)
	}

	for _, value := range []string{"", "xx", "en-XA"} {
		cleared := pick("lang=" + value).Result().Cookies()
		if len(cleared) != 1 || cleared[0].MaxAge >= 0 {
			t.Errorf("lang=%q should clear the choice, got %+v", value, cleared)
		}
	}
}

func TestEveryPageSpeaksChineseWhereTheCatalogDoes(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	seedPseudoFixtures(t, pipeline)

	for _, path := range []string{"/p/demo", "/p/demo/runs", "/p/demo/resources", "/p/demo/approvals", "/p/demo/questions", "/p/demo/jobs/build/detail", "/p/demo/runs/fx1", "/nosuch"} {
		_, page := getAs(t, server, path, http.Header{"Accept-Language": {"zh-CN"}})

		for _, english := range []string{">runs<", ">approvals<", "skip to content", "Trigger queue", "back to jobs", " ago<"} {
			if strings.Contains(page, english) {
				t.Errorf("%s: still says %q in Chinese", path, english)
			}
		}
	}
}

func TestEachLanguageIsMatchedAndSpoken(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	seedPseudoFixtures(t, pipeline)

	for ask, tag := range map[string]string{
		"ja": "ja", "ja-JP": "ja", "es": "es", "es-MX": "es", "gl": "es", "pt": "pt-BR", "pt-PT": "pt-BR", "pt-BR": "pt-BR",
	} {
		for _, path := range []string{"/p/demo", "/p/demo/runs", "/p/demo/approvals", "/p/demo/runs/fx1", "/nosuch"} {
			_, page := getAs(t, server, path, http.Header{"Accept-Language": {ask}})

			if !strings.Contains(page, `<html lang="`+tag+`">`) {
				t.Errorf("%s as %q: not served in %s", path, ask, tag)
			}

			for _, english := range []string{"skip to content", "Trigger queue", "back to jobs", ">approvals<", " ago<", ">runs<"} {
				if strings.Contains(page, english) {
					t.Errorf("%s as %q: still says %q", path, ask, english)
				}
			}
		}
	}
}
