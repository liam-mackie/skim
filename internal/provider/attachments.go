package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"skim/internal/domain"
)

const (
	htmlMediaType     = "application/vnd.github.html+json"
	maxAttachmentURLs = 1000
	maxAttachmentURL  = 4096
)

var attachmentUUID = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// AttachmentID returns the asset UUID of a GitHub image attachment URL, as
// written in Markdown (github.com/user-attachments/assets/UUID, legacy
// user-images) or as signed by GitHub (private-user-images/.../ID-UUID.ext).
func AttachmentID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return ""
	}
	var candidate string
	switch u.Host {
	case "github.com":
		rest, ok := strings.CutPrefix(u.Path, "/user-attachments/assets/")
		if !ok || strings.Contains(rest, "/") {
			return ""
		}
		candidate = rest
	case "user-images.githubusercontent.com", "private-user-images.githubusercontent.com":
		base := path.Base(u.Path)
		_, candidate, _ = strings.Cut(strings.TrimSuffix(base, path.Ext(base)), "-")
	default:
		return ""
	}
	if !attachmentUUID.MatchString(candidate) || len(candidate) != 36 {
		return ""
	}
	return strings.ToLower(candidate)
}

// signedAttachments collects GitHub's pre-signed image URLs from rendered HTML.
// Only https private-user-images URLs carrying a jwt are accepted.
func signedAttachments(rendered string, into map[string]string) {
	tokens := html.NewTokenizer(strings.NewReader(rendered))
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			return
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := tokens.TagName()
			if string(name) != "img" {
				continue
			}
			for hasAttr {
				var key, value []byte
				key, value, hasAttr = tokens.TagAttr()
				if string(key) != "src" || len(value) > maxAttachmentURL || len(into) >= maxAttachmentURLs {
					continue
				}
				src := string(value)
				u, err := url.Parse(src)
				if err != nil || u.Host != "private-user-images.githubusercontent.com" || u.Query().Get("jwt") == "" {
					continue
				}
				if id := AttachmentID(src); id != "" {
					into[id] = src
				}
			}
		}
	}
}

// Attachments returns GitHub's short-lived (about five minutes) pre-signed image
// URLs for the PR description and review comments, keyed by asset UUID. They let
// private-repository attachments load without a GitHub browser session.
func (g *GitHub) Attachments(ctx context.Context, pr domain.PullRequest) (domain.Attachments, error) {
	result := domain.Attachments{PRIdentity: domain.PRIdentity{PRKey: pr.Key(), BaseSHA: pr.BaseSHA, HeadSHA: pr.HeadSHA}, URLs: map[string]string{}}
	p, err := pullPath(pr)
	if err != nil {
		return result, err
	}
	u, err := g.endpoint(p, nil)
	if err != nil {
		return result, err
	}
	var pull struct {
		Number   int    `json:"number"`
		BodyHTML string `json:"body_html"`
		Base     struct {
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if _, err := g.requestAs(ctx, http.MethodGet, u, htmlMediaType, nil, &pull); err != nil {
		return result, err
	}
	if pull.Number != pr.Number || pull.Base.SHA != pr.BaseSHA || pull.Head.SHA != pr.HeadSHA {
		return result, fmt.Errorf("pull-request head or base changed while loading attachments; reload the pull request")
	}
	signedAttachments(pull.BodyHTML, result.URLs)
	for page := 1; ; page++ {
		u, err := g.endpoint(p+"/comments", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}})
		if err != nil {
			return result, err
		}
		var batch []struct {
			BodyHTML string `json:"body_html"`
		}
		header, err := g.requestAs(ctx, http.MethodGet, u, htmlMediaType, nil, &batch)
		if err != nil {
			return result, err
		}
		if batch == nil || len(batch) > 100 {
			return result, fmt.Errorf("GitHub returned an invalid review-comment page")
		}
		for _, comment := range batch {
			signedAttachments(comment.BodyHTML, result.URLs)
		}
		next, err := nextPage(header, u, page)
		if err != nil {
			return result, err
		}
		if !next {
			return result, nil
		}
		if len(batch) == 0 || page == maxReviewPages {
			return result, fmt.Errorf("GitHub review-comment pagination is incomplete or exceeds 100 pages")
		}
	}
}
