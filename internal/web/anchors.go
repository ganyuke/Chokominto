package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// After a button is used, the page comes back to where the button was
// instead of the top. Every form that changes something carries the id of
// the heading or row it sits under (markForms adds it when a page is
// rendered, so templates don't repeat it), and the redirect that follows
// gets that id as its fragment (jumpBack). Handlers that name a place
// themselves keep theirs.

// markable finds the places a page can come back to, and the forms that
// change something.
var markable = regexp.MustCompile(`<(?:h[1-6]|tr|li|section|details)\b[^>]*?\sid="([^"]+)"[^>]*>|<form\b[^>]*\smethod="post"[^>]*>`)

var anchorName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// markForms adds a hidden "at" field to each post form, naming the nearest
// heading or row with an id before it. Forms above the first one (the
// top bar, the notice) get none.
func markForms(page string) string {
	at := ""
	return markable.ReplaceAllStringFunc(page, func(tag string) string {
		if !strings.HasPrefix(tag, "<form") {
			at = markable.FindStringSubmatch(tag)[1]
			return tag
		}
		if !anchorName.MatchString(at) {
			return tag
		}
		return tag + `<input type="hidden" name="at" value="` + at + `">`
	})
}

// jumpBack sends a form's redirect to where its button was, when the
// redirect leads back to the page the form was on and names no place of
// its own.
type jumpBack struct {
	http.ResponseWriter
	r *http.Request
}

func (j jumpBack) WriteHeader(status int) {
	if loc := j.Header().Get("Location"); status == http.StatusSeeOther && loc != "" && !strings.Contains(loc, "#") {
		at := j.r.PostFormValue("at")
		to, err1 := url.Parse(loc)
		from, err2 := url.Parse(j.r.Referer())
		if anchorName.MatchString(at) && err1 == nil && err2 == nil && (from.Path == "" || from.Path == to.Path) {
			j.Header().Set("Location", loc+"#"+at)
		}
	}
	j.ResponseWriter.WriteHeader(status)
}
