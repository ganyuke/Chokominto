package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The readings on by default, as the Settings form sends them.
var defaultReadings = []string{"feat", "comma", "slash", "cv", "group", "titles", "version", "covers", "compilation"}

func readingsForm(except ...string) url.Values {
	v := url.Values{}
	for _, k := range defaultReadings {
		if !strings.Contains(strings.Join(except, " "), k) {
			v.Add("on", k)
		}
	}
	return v
}

func TestReadingSettings(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	e.scrobbleNow([3]string{"CHiCO, HoneyWorks", "ツーマンライブ", ""})
	_, body, _ := e.get("/settings")
	for _, want := range []string{"Reading scrobbles", "Lists with commas", "Asami Seto, Nao Toyama → two artists",
		`name="on" value="comma" checked`, `name="on" value="amp">`, "Titles in two languages", "Your rules", "None yet."} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings missing %q", want)
		}
	}

	// Saving in Settings only shows what would change.
	code, body, _ := e.get("/settings/readings?" + readingsForm("comma").Encode())
	if code != http.StatusOK {
		t.Fatalf("preview: %d", code)
	}
	for _, want := range []string{"Turn off: Lists with commas", "CHiCO, HoneyWorks — ツーマンライブ",
		"<td class=\"muted\">CHiCO · HoneyWorks — ツーマンライブ</td>", "<td>CHiCO, HoneyWorks — ツーマンライブ</td>",
		`<input type="hidden" name="on" value="feat">`} {
		if !strings.Contains(body, want) {
			t.Fatalf("preview missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `value="comma"`) {
		t.Fatal("preview keeps commas on")
	}
	if _, body, _ = e.get("/settings"); !strings.Contains(body, `value="comma" checked`) {
		t.Fatal("preview changed the setting")
	}

	code, _, h := e.post("/settings/readings", readingsForm("comma"))
	if code != http.StatusSeeOther {
		t.Fatalf("save: %d", code)
	}
	_, body, _ = e.get(h.Get("Location"))
	if !strings.Contains(body, "Turned off reading: Lists with commas.") || !strings.Contains(body, `name="on" value="comma">`) {
		t.Fatal("not switched off")
	}

	// Nothing to change, nothing saved.
	for _, try := range []func() (int, string, http.Header){
		func() (int, string, http.Header) {
			return e.get("/settings/readings?" + readingsForm("comma").Encode())
		},
		func() (int, string, http.Header) { return e.post("/settings/readings", readingsForm("comma")) },
	} {
		code, _, h := try()
		if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "readings-same") {
			t.Fatalf("no change: %d %s", code, h.Get("Location"))
		}
	}

	// Two at once is one edit.
	form := readingsForm()
	form.Add("on", "amp")
	if _, body, _ = e.get("/settings/readings?" + form.Encode()); !strings.Contains(body, "Turn on: Lists with commas") || !strings.Contains(body, "Turn on: Lists with &amp;") {
		t.Fatalf("two changes:\n%s", body)
	}
	_, _, h = e.post("/settings/readings", form)
	if _, body, _ = e.get(h.Get("Location")); !strings.Contains(body, "Turned on reading: Lists with commas. Turned on reading: Lists with &amp;.") {
		t.Fatalf("one edit:\n%s", body)
	}

	// Built-in readings can't be deleted as rules.
	if code, _, _ := e.post("/settings/rules/1", url.Values{"action": {"delete"}}); code != http.StatusNotFound {
		t.Fatalf("deleting a reading: %d", code)
	}
}

func TestOtherNamesSetting(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	e.scrobbleNow([3]string{"sajou no hana", "君のせい - Kiminosei", ""})
	other := `<span class="other">君のせい</span>`
	if _, body, _ := e.get("/history"); !strings.Contains(body, "Kiminosei") || strings.Contains(body, "君のせい") {
		t.Fatalf("other names shown by default:\n%s", body)
	}
	if code, _, _ := e.post("/settings/names", url.Values{"on": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("show: %d", code)
	}
	if _, body, _ := e.get("/history"); !strings.Contains(body, other) {
		t.Fatalf("other names not shown:\n%s", body)
	}
	// Visitors see what the owner chose.
	e.post("/logout", nil)
	if _, body, _ := e.get("/history"); !strings.Contains(body, other) {
		t.Fatal("other names not shown to visitors")
	}
	e.login()
	e.post("/settings/names", url.Values{"on": {"0"}})
	if _, body, _ := e.get("/history"); strings.Contains(body, "君のせい") {
		t.Fatal("other names still shown")
	}
}

func TestVersionBeforeOtherNames(t *testing.T) {
	e := newEnv(t, true)
	e.login()
	e.scrobbleNow([3]string{"YURiKA", "鏡面の波 - Kyoumen no Nami", ""}, [3]string{"YURiKA", "鏡面の波 (Instrumental)", ""})
	e.post("/settings/names", url.Values{"on": {"1"}})
	_, body, _ := e.get("/history")
	want := `Kyoumen no Nami</a> <span class="muted">(Instrumental)</span> <span class="other">鏡面の波</span>`
	if !strings.Contains(body, want) {
		t.Fatalf("version not next to the name:\n%s", body)
	}
}
