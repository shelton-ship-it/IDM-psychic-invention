package hlsengine

import (
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"streamvault/core/domain"
)

func testMPDURL(t *testing.T) *url.URL {
	t.Helper()
	u, _ := url.Parse("https://cdn.example.com/video/dir/manifest.mpd")
	return u
}

const mpdHead = `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT10S">`

func segURLs(p *Playlist) []string {
	var o []string
	for _, s := range p.Segments {
		o = append(o, s.URL)
	}
	return o
}

func TestParseMPDRealFFmpegManifest(t *testing.T) {
	raw, err := os.ReadFile("../mp4merge/testdata/dash/manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := ParseMPD(string(raw), testMPDURL(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 2 {
		t.Fatalf("esperava vídeo + áudio, obtido %d faixas", len(tr))
	}
	v, a := tr[0], tr[1]
	if v.MapURL != "https://cdn.example.com/video/dir/init-stream0.m4s" || a.MapURL != "https://cdn.example.com/video/dir/init-stream1.m4s" {
		t.Errorf("inits: %q %q", v.MapURL, a.MapURL)
	}
	want := []string{"https://cdn.example.com/video/dir/chunk-stream0-00001.m4s", "https://cdn.example.com/video/dir/chunk-stream0-00002.m4s", "https://cdn.example.com/video/dir/chunk-stream0-00003.m4s"}
	if got := segURLs(v); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("vídeo (r=2 → 3 segmentos, $Number%%05d$): %v", got)
	}
	if len(a.Segments) != 3 || !strings.HasSuffix(a.Segments[2].URL, "chunk-stream1-00003.m4s") {
		t.Errorf("áudio (S sem t e sem r): %v", segURLs(a))
	}
	if v.Segments[0].Duration != 2 || !isFMP4(v) || !isFMP4(a) || v.Ext != ".mp4" || a.Ext != ".m4a" {
		t.Errorf("duração/contentor/extensão: %+v %+v", v.Segments[0], v)
	}
	if !v.EndList {
		t.Error("DASH estático é VOD (EndList)")
	}
}

func TestParseMPDAddressingModes(t *testing.T) {
	cases := map[string]struct {
		body string
		want []string
		init string
	}{
		"$Time$ com repetição": {
			`<Period><AdaptationSet contentType="video"><Representation id="v" mimeType="video/mp4" bandwidth="5"><BaseURL>b/</BaseURL>
<SegmentTemplate timescale="1000" initialization="i-$RepresentationID$.mp4" media="$RepresentationID$/$Time$.m4s"><SegmentTimeline>
<S t="0" d="2000" r="1"/><S d="3000"/></SegmentTimeline></SegmentTemplate></Representation></AdaptationSet></Period>`,
			[]string{"https://cdn.example.com/video/dir/b/v/0.m4s", "https://cdn.example.com/video/dir/b/v/2000.m4s", "https://cdn.example.com/video/dir/b/v/4000.m4s"},
			"https://cdn.example.com/video/dir/b/i-v.mp4"},
		"duração fixa ($Number$, startNumber)": {
			`<Period><AdaptationSet contentType="video"><Representation id="1" mimeType="video/mp4" bandwidth="5">
<SegmentTemplate timescale="1" duration="4" startNumber="7" initialization="init.mp4" media="s$Number$.m4s"/></Representation></AdaptationSet></Period>`,
			[]string{"https://cdn.example.com/video/dir/s7.m4s", "https://cdn.example.com/video/dir/s8.m4s", "https://cdn.example.com/video/dir/s9.m4s"}, // 10 s / 4 s = 3
			"https://cdn.example.com/video/dir/init.mp4"},
		"r negativo até ao fim": {
			`<Period duration="PT6S"><AdaptationSet contentType="video"><Representation id="1" mimeType="video/mp4" bandwidth="5">
<SegmentTemplate timescale="1" media="s$Number$.m4s"><SegmentTimeline><S t="0" d="2" r="-1"/></SegmentTimeline></SegmentTemplate></Representation></AdaptationSet></Period>`,
			[]string{"https://cdn.example.com/video/dir/s1.m4s", "https://cdn.example.com/video/dir/s2.m4s", "https://cdn.example.com/video/dir/s3.m4s"}, ""},
		"SegmentList": {
			`<Period><AdaptationSet contentType="audio"><Representation id="1" mimeType="audio/mp4" bandwidth="5"><SegmentList timescale="1" duration="2">
<Initialization sourceURL="http://other.example.net/i.mp4"/><SegmentURL media="a/1.m4s"/><SegmentURL media="a/2.m4s"/></SegmentList></Representation></AdaptationSet></Period>`,
			[]string{"https://cdn.example.com/video/dir/a/1.m4s", "https://cdn.example.com/video/dir/a/2.m4s"}, "http://other.example.net/i.mp4"},
		"ficheiro único (BaseURL)": {
			`<Period><AdaptationSet contentType="video"><Representation id="1" mimeType="video/mp4" bandwidth="5"><BaseURL>../all/video.mp4</BaseURL></Representation></AdaptationSet></Period>`,
			[]string{"https://cdn.example.com/video/all/video.mp4"}, ""},
		"BaseURL encadeado (MPD → Período → Adaptação → Representação)": {
			`<Period><BaseURL>p/</BaseURL><AdaptationSet contentType="video"><BaseURL>a/</BaseURL><Representation id="1" mimeType="video/mp4" bandwidth="5"><BaseURL>r/</BaseURL>
<SegmentTemplate timescale="1" duration="10" media="s$Number$.m4s"/></Representation></AdaptationSet></Period>`,
			[]string{"https://cdn.example.com/video/dir/m/p/a/r/s1.m4s"}, ""},
		"modelo herdado da adaptação": {
			`<Period><AdaptationSet contentType="video"><SegmentTemplate timescale="1" duration="10" media="$Bandwidth$-$Number%03d$.m4s"/>
<Representation id="1" mimeType="video/mp4" bandwidth="777"/></AdaptationSet></Period>`,
			[]string{"https://cdn.example.com/video/dir/777-001.m4s"}, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			text := mpdHead
			if strings.Contains(name, "encadeado") {
				text = strings.Replace(mpdHead, "static\"", "static\"", 1) + "<BaseURL>m/</BaseURL>"
			}
			tr, err := ParseMPD(text+c.body+"</MPD>", testMPDURL(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(tr) != 1 {
				t.Fatalf("faixas: %d", len(tr))
			}
			if got := segURLs(tr[0]); strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("segmentos:\n got %v\nwant %v", got, c.want)
			}
			if tr[0].MapURL != c.init {
				t.Errorf("init %q, esperado %q", tr[0].MapURL, c.init)
			}
		})
	}
}

func TestParseMPDChoosesBestVideoAndMainAudio(t *testing.T) {
	rep := func(id, mime string, bw, h int) string {
		return `<Representation id="` + id + `" mimeType="` + mime + `" bandwidth="` + itoa(bw) + `" height="` + itoa(h) + `"><SegmentTemplate timescale="1" duration="10" media="` + id + `-$Number$.m4s"/></Representation>`
	}
	body := `<Period>
<AdaptationSet contentType="video">` + rep("v-low", "video/mp4", 100, 240) + rep("v-high", "video/mp4", 900, 1080) + rep("v-mid", "video/mp4", 500, 720) + `</AdaptationSet>
<AdaptationSet contentType="audio" lang="en">` + rep("a-en", "audio/mp4", 64, 0) + `</AdaptationSet>
<AdaptationSet contentType="audio" lang="pt"><Role value="main"/>` + rep("a-pt-lo", "audio/mp4", 32, 0) + rep("a-pt-hi", "audio/mp4", 128, 0) + `</AdaptationSet>
<AdaptationSet contentType="text">` + rep("t", "text/vtt", 1, 0) + `</AdaptationSet>
</Period>`
	tr, err := ParseMPD(mpdHead+body+"</MPD>", testMPDURL(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 2 || !strings.Contains(tr[0].Segments[0].URL, "v-high-1") {
		t.Fatalf("vídeo de maior débito: %v", tr)
	}
	if !strings.Contains(tr[1].Segments[0].URL, "a-pt-hi-1") {
		t.Errorf("áudio: preferir o AdaptationSet com Role=main e, dentro dele, o de maior débito: %v", segURLs(tr[1]))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestParseMPDRefusalsAndInvalidInput(t *testing.T) {
	seg := `<SegmentTemplate timescale="1" duration="10" media="s$Number$.m4s"/>`
	ok := func(extra string) string {
		return mpdHead + `<Period>` + extra + `<AdaptationSet contentType="video"><Representation id="1" mimeType="video/mp4" bandwidth="1">` + seg + `</Representation></AdaptationSet></Period></MPD>`
	}
	type tc struct {
		name, doc string
		typed     bool // erro tipado (recusa por desenho) vs entrada inválida
	}
	cases := []tc{
		{"dinâmico", strings.Replace(ok(""), `type="static"`, `type="dynamic"`, 1), true},
		{"DRM no período", ok(`<ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"/>`), true},
		{"DRM na adaptação", strings.Replace(ok(""), `<AdaptationSet contentType="video">`, `<AdaptationSet contentType="video"><ContentProtection schemeIdUri="x"/>`, 1), true},
		{"DRM na representação", strings.Replace(ok(""), `<SegmentTemplate`, `<ContentProtection schemeIdUri="x"/><SegmentTemplate`, 1), true},
		{"vários períodos", mpdHead + `<Period/><Period/></MPD>`, true},
		{"sem faixas", mpdHead + `<Period><AdaptationSet contentType="text"><Representation id="1" mimeType="text/vtt" bandwidth="1"/></AdaptationSet></Period></MPD>`, true},
		{"contentor MPEG-TS", strings.Replace(ok(""), "video/mp4", "video/mp2t", 1), true},
		{"intervalo de bytes", strings.Replace(ok(""), seg, `<SegmentList timescale="1" duration="1"><SegmentURL media="a.mp4" mediaRange="0-99"/></SegmentList>`, 1), true},
		{"sem períodos", mpdHead + `</MPD>`, false},
		{"XML inválido", `<MPD><Period>`, false},
		{"não é XML", `#EXTM3U`, false},
		{"identificador desconhecido", strings.Replace(ok(""), "s$Number$.m4s", "s$Foo$.m4s", 1), false},
		{"formato inválido", strings.Replace(ok(""), "s$Number$.m4s", "s$Number%d%d$.m4s", 1), false},
		{"$ por fechar", strings.Replace(ok(""), "s$Number$.m4s", "s$Number.m4s", 1), false},
		{"esquema não http", strings.Replace(ok(""), "s$Number$.m4s", "file:///etc/passwd?$Number$", 1), false},
		{"sem duração", strings.Replace(strings.Replace(ok(""), ` mediaPresentationDuration="PT10S"`, "", 1), `duration="10"`, `duration="10"`, 1), false},
		{"duração inválida", strings.Replace(ok(""), "PT10S", "dez segundos", 1), false},
		{"segmentos a mais", strings.Replace(ok(""), `duration="10"`, `duration="1"`, 1), false}, // 10 s / 1 s = 10: ok → ajustado abaixo
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := c.doc
			if c.name == "segmentos a mais" {
				doc = strings.Replace(strings.Replace(ok(""), `duration="10"`, `duration="1"`, 1), "PT10S", "PT999999999S", 1)
			}
			_, err := ParseMPD(doc, testMPDURL(t))
			if err == nil {
				t.Fatal("devia falhar")
			}
			_, typed := err.(*domain.DownloadError)
			if typed != c.typed {
				t.Errorf("tipo do erro: tipado=%v esperado=%v (%v)", typed, c.typed, err)
			}
			if typed && err.(*domain.DownloadError).Action == "" {
				t.Errorf("recusas por desenho têm de dizer o que fazer: %v", err)
			}
		})
	}
}

func TestMPDHelpers(t *testing.T) {
	for in, want := range map[string]float64{"PT10S": 10, "PT1H2M3.5S": 3723.5, "P1DT1H": 90000, "PT0S": 0, "PT2M": 120} {
		if got, err := parseISODuration(in); err != nil || got != want {
			t.Errorf("%s = %v (%v), esperado %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "P", "PT", "10", "P1Y", "P2M", "PT-5S", "PTxS"} {
		if _, err := parseISODuration(bad); err == nil {
			t.Errorf("%q devia ser recusado", bad)
		}
	}
	got, err := expand("a/$RepresentationID$/$$/$Number%05d$-$Time$-$Bandwidth$", "R1", 9, 42, 777)
	if err != nil || got != "a/R1/$/00042-777-9" {
		t.Errorf("expand: %q %v", got, err)
	}
}
