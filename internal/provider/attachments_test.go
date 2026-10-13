package provider

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const (
	bodyUUID    = "0d1e2f3a-4b5c-4d6e-8f90-a1b2c3d4e5f6"
	commentUUID = "11111111-2222-4333-8444-555555555555"
)

func signed(uuid, jwt string) string {
	return fmt.Sprintf("https://private-user-images.githubusercontent.com/42/900-%s.png?jwt=%s", uuid, jwt)
}

func TestAttachmentID(t *testing.T) {
	for raw, want := range map[string]string{
		"https://github.com/user-attachments/assets/" + strings.ToUpper(bodyUUID): bodyUUID,
		"https://user-images.githubusercontent.com/42/900-" + bodyUUID + ".png":   bodyUUID,
		signed(bodyUUID, "token"):                                           bodyUUID,
		"https://github.com/user-attachments/files/12/report.pdf":           "",
		"https://github.com/user-attachments/assets/" + bodyUUID + "/extra": "",
		"http://github.com/user-attachments/assets/" + bodyUUID:             "",
		"https://evil.example/user-attachments/assets/" + bodyUUID:          "",
		"https://camo.githubusercontent.com/abc/" + bodyUUID:                "",
		"https://user:pass@github.com/user-attachments/assets/" + bodyUUID:  "",
		"https://github.com/user-attachments/assets/" + bodyUUID + "0":      "",
	} {
		if got := AttachmentID(raw); got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
}

func TestAttachmentsCollectSignedURLsFromRenderedBodies(t *testing.T) {
	var accepts []string
	g := fixture(t, "", func(w http.ResponseWriter, r *http.Request) {
		accepts = append(accepts, r.Header.Get("Accept"))
		switch {
		case r.URL.Path == "/repos/owner/repo/pulls/7":
			pull := pullFixture("head-sha", 1)
			// Escaped ampersands, a camo badge, an unsigned private URL and a lookalike host are all realistic noise.
			pull["body_html"] = `<p><a href="x"><img src="` + strings.ReplaceAll(signed(bodyUUID, "body&x=1"), "&", "&amp;") + `" alt="shot"></a>` +
				`<img src="https://camo.githubusercontent.com/abc/def" data-canonical-src="https://example.com/badge.svg">` +
				`<img src="https://private-user-images.githubusercontent.com/42/901-` + commentUUID + `.png">` +
				`<img src="https://private-user-images.githubusercontent.com.evil.example/42/902-` + commentUUID + `.png?jwt=x"></p>`
			respondJSON(t, w, pull)
		case r.URL.Path == "/repos/owner/repo/pulls/7/comments" && r.URL.Query().Get("page") == "1":
			w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/owner/repo/pulls/7/comments?page=2>; rel="next"`)
			respondJSON(t, w, []any{map[string]any{"body_html": "<p>no images</p>"}})
		case r.URL.Path == "/repos/owner/repo/pulls/7/comments":
			respondJSON(t, w, []any{map[string]any{"body_html": `<img src="` + signed(commentUUID, "comment") + `">`}})
		default:
			t.Errorf("unexpected request %s", r.URL)
		}
	})
	pr := loadedPR()
	pr.BaseSHA = "base-sha"
	got, err := g.Attachments(t.Context(), pr)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{bodyUUID: signed(bodyUUID, "body&x=1"), commentUUID: signed(commentUUID, "comment")}
	if len(got.URLs) != 2 || got.URLs[bodyUUID] != want[bodyUUID] || got.URLs[commentUUID] != want[commentUUID] || got.PRKey != pr.Key() || got.HeadSHA != "head-sha" {
		t.Fatalf("got %+v", got)
	}
	for _, accept := range accepts {
		if accept != "application/vnd.github.html+json" {
			t.Fatalf("rendered bodies need the html media type, got %q", accept)
		}
	}
	if len(accepts) != 3 {
		t.Fatalf("expected PR plus two comment pages, got %d requests", len(accepts))
	}

	pr.HeadSHA = "older-head"
	if _, err := g.Attachments(t.Context(), pr); err == nil || !strings.Contains(err.Error(), "reload") {
		t.Fatalf("a moved head must not return another snapshot's URLs: %v", err)
	}
}
