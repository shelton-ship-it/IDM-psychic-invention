package hlsengine

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func mustBase(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

const master = "\ufeff#EXTM3U\r\n#EXT-X-VERSION:6\r\n" +
	`#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2"` + "\r\nlow/index.m3u8\r\n" +
	`#EXT-X-STREAM-INF:BANDWIDTH=2800000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2"` + "\r\nhttps://cdn.example.com/hd/index.m3u8?tok=1\r\n" +
	`#EXT-X-STREAM-INF:BANDWIDTH=1400000,RESOLUTION=842x480` + "\r\n../mid/index.m3u8\r\n" +
	`#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=90000,URI="iframe.m3u8"` + "\r\n"

func TestMasterPlaylistPicksHighestBandwidthAndResolvesURLs(t *testing.T) {
	p, err := Parse(master, mustBase(t, "https://site.example.com/v/1/master.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Master || len(p.Variants) != 3 {
		t.Fatalf("master: %+v", p)
	}
	best, err := p.pickVariant()
	if err != nil || best.Bandwidth != 2800000 || best.Height != 720 || best.URL != "https://cdn.example.com/hd/index.m3u8?tok=1" {
		t.Fatalf("variante: %+v %v", best, err)
	}
	// URLs relativos resolvidos contra a base (incluindo ../)
	if p.Variants[0].URL != "https://site.example.com/v/1/low/index.m3u8" || p.Variants[2].URL != "https://site.example.com/v/mid/index.m3u8" {
		t.Errorf("resolução de URLs: %+v", p.Variants)
	}
}

func TestMediaPlaylistSegmentsMapAndFlags(t *testing.T) {
	text := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6.0,\nseg1.m4s\n#EXTINF:5.5,titulo\nseg2.m4s?x=1\n#EXT-X-ENDLIST\n"
	p, err := Parse(text, mustBase(t, "https://a.example.com/p/media.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Master || !p.EndList || len(p.Segments) != 2 || p.MapURL != "https://a.example.com/p/init.mp4" ||
		p.Segments[1].URL != "https://a.example.com/p/seg2.m4s?x=1" || p.Segments[1].Duration != 5.5 {
		t.Fatalf("%+v", p)
	}
	if p.KeyMethod != "" || p.ByteRange {
		t.Errorf("flags: %+v", p)
	}
}

func TestDetectsEncryptionLiveByteRangeAndAudioRenditions(t *testing.T) {
	base := mustBase(t, "https://a.example.com/m.m3u8")
	enc, _ := Parse("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k.key\"\n#EXTINF:4,\ns.ts\n#EXT-X-ENDLIST\n", base)
	if enc.KeyMethod != "AES-128" {
		t.Errorf("cifra não detetada: %q", enc.KeyMethod)
	}
	none, _ := Parse("#EXTM3U\n#EXT-X-KEY:METHOD=NONE\n#EXTINF:4,\ns.ts\n#EXT-X-ENDLIST\n", base)
	if none.KeyMethod != "" {
		t.Errorf("METHOD=NONE não é cifra: %q", none.KeyMethod)
	}
	live, _ := Parse("#EXTM3U\n#EXTINF:4,\ns.ts\n", base)
	if live.EndList {
		t.Error("sem ENDLIST é transmissão ao vivo")
	}
	br, _ := Parse("#EXTM3U\n#EXT-X-BYTERANGE:1000@0\n#EXTINF:4,\ns.ts\n#EXT-X-ENDLIST\n", base)
	if !br.ByteRange {
		t.Error("byte-range não detetado")
	}
	m, _ := Parse("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"pt\",URI=\"audio.m3u8\"\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=1000,AUDIO=\"aud\"\nv.m3u8\n", base)
	v, err := m.pickVariant()
	if err != nil {
		t.Fatalf("a variante com áudio em separado é escolhível: %v", err)
	}
	a := m.pickAudio(v)
	if a == nil || a.URL != "https://a.example.com/audio.m3u8" || a.Name != "pt" {
		t.Errorf("devia escolher a faixa de áudio com o URI resolvido: %+v", a)
	}
	// o mesmo grupo de áudio SEM uri (áudio dentro do vídeo): não há faixa à parte
	ok, _ := Parse("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"pt\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000,AUDIO=\"aud\"\nv.m3u8\n", base)
	okv, err := ok.pickVariant()
	if err != nil || ok.pickAudio(okv) != nil {
		t.Errorf("áudio embebido: sem faixa separada (%v)", err)
	}
}

func TestPickAudioPrefersDefaultThenFirstAndIgnoresOtherGroups(t *testing.T) {
	base := mustBase(t, "https://a.example.com/m.m3u8")
	m, err := Parse("#EXTM3U\n"+
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a1\",NAME=\"en\",LANGUAGE=\"en\",URI=\"en.m3u8\"\n"+
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a1\",NAME=\"pt\",LANGUAGE=\"pt\",DEFAULT=YES,URI=\"pt.m3u8\"\n"+
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a2\",NAME=\"x\",URI=\"x.m3u8\"\n"+
		"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"s\",NAME=\"leg\",URI=\"leg.m3u8\"\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=1000,AUDIO=\"a1\"\nv.m3u8\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=500,AUDIO=\"a2\"\nlow.m3u8\n", base)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := m.pickVariant()
	if a := m.pickAudio(v); a == nil || a.Language != "pt" || !a.Default {
		t.Errorf("devia preferir a faixa DEFAULT=YES do grupo da variante: %+v", a)
	}
	if a := m.pickAudio(&Variant{AudioGrp: "a2"}); a == nil || a.Name != "x" {
		t.Errorf("sem DEFAULT: a primeira do grupo: %+v", a)
	}
	if m.pickAudio(&Variant{AudioGrp: "nao-existe"}) != nil || m.pickAudio(&Variant{}) != nil || m.pickAudio(nil) != nil {
		t.Error("grupo inexistente/vazio: sem áudio")
	}
}

func TestAttributeParsingHandlesQuotedCommas(t *testing.T) {
	a := attrs(`BANDWIDTH=123,CODECS="avc1.4d,mp4a.40",RESOLUTION=1x2,NAME="a=b"`)
	if a["BANDWIDTH"] != "123" || a["CODECS"] != "avc1.4d,mp4a.40" || a["RESOLUTION"] != "1x2" || a["NAME"] != "a=b" {
		t.Fatalf("%v", a)
	}
}

func TestParseRejectsHostileAndMalformedInput(t *testing.T) {
	base := mustBase(t, "https://a.example.com/m.m3u8")
	var many strings.Builder
	many.WriteString("#EXTM3U\n")
	for i := 0; i <= maxSegments; i++ {
		fmt.Fprintf(&many, "#EXTINF:1,\ns%d.ts\n", i)
	}
	for name, text := range map[string]string{
		"sem EXTM3U":         "<html>não é playlist</html>",
		"vazia":              "",
		"só espaços":         "  \n\n ",
		"esquema javascript": "#EXTM3U\n#EXTINF:4,\njavascript:alert(1)\n",
		"esquema file":       "#EXTM3U\n#EXTINF:4,\nfile:///etc/passwd\n",
		"esquema ftp":        "#EXTM3U\n#EXTINF:4,\nftp://x.example.com/a.ts\n",
		"linha enorme":       "#EXTM3U\n#EXTINF:4,\n" + strings.Repeat("a", maxLineLen+1) + "\n",
		"demasiados segm.":   many.String(),
		"map inválido":       "#EXTM3U\n#EXT-X-MAP:URI=\"data:text/plain,x\"\n",
	} {
		if _, err := Parse(text, base); err == nil {
			t.Errorf("%s: devia ser recusado", name)
		}
	}
	if _, err := Parse(strings.Repeat("x", maxPlaylistBytes+1), base); err == nil {
		t.Error("playlist gigante devia ser recusada")
	}
	// um segmento "#EXTINF" sem URI a seguir não cria segmento fantasma
	p, err := Parse("#EXTM3U\n#EXTINF:4,\n#EXT-X-ENDLIST\n", base)
	if err != nil || len(p.Segments) != 0 {
		t.Errorf("%+v %v", p, err)
	}
}
