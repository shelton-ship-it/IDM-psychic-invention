package hlsengine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"

	"streamvault/core/domain"
	"streamvault/core/engine"
)

// Fixtures reais gerados com o ffmpeg (ver mp4merge/testdata).
const fixHLS = "../mp4merge/testdata/hls/"
const fixDASH = "../mp4merge/testdata/dash/"

// siteTree monta uma árvore de ficheiros num diretório temporário e serve-a,
// contando os pedidos por caminho; opcionalmente bloqueia caminhos até abrir o gate.
type siteTree struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int

	gateMu   sync.Mutex
	gateOpen chan struct{}
	gated    func(path string) bool
}

func newSite(t *testing.T, files map[string][]byte) *siteTree {
	t.Helper()
	s := &siteTree{hits: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		s.mu.Unlock()
		s.gateMu.Lock()
		gate, gated := s.gateOpen, s.gated
		s.gateMu.Unlock()
		if gate != nil && gated != nil && gated(r.URL.Path) {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, ".m3u8"):
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		case strings.HasSuffix(r.URL.Path, ".mpd"):
			w.Header().Set("Content-Type", "application/dash+xml")
		}
		w.Write(b)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *siteTree) hitsMatching(prefix, suffix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p, c := range s.hits {
		if strings.HasPrefix(p, prefix) && strings.HasSuffix(p, suffix) {
			n += c
		}
	}
	return n
}

func (s *siteTree) setGate(gated func(string) bool) chan struct{} {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()
	s.gateOpen, s.gated = make(chan struct{}), gated
	return s.gateOpen
}

func rd(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// hlsSeparateAudioSite: master com #EXT-X-MEDIA de áudio + playlists fMP4 reais.
func hlsSeparateAudioSite(t *testing.T) *siteTree {
	t.Helper()
	files := map[string][]byte{
		"/master.m3u8": []byte("#EXTM3U\n" +
			"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"en\",LANGUAGE=\"en\",DEFAULT=YES,URI=\"audio/a.m3u8\"\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=200000,RESOLUTION=160x120,AUDIO=\"aud\"\nvideo/v.m3u8\n"),
		"/video/v.m3u8": rd(t, fixHLS+"v.m3u8"),
		"/audio/a.m3u8": rd(t, fixHLS+"a.m3u8"),
	}
	for _, n := range []string{"v-init.mp4", "v0.m4s", "v1.m4s", "v2.m4s"} {
		files["/video/"+n] = rd(t, fixHLS+n)
	}
	for _, n := range []string{"a-init.mp4", "a0.m4s", "a1.m4s", "a2.m4s", "a3.m4s"} {
		files["/audio/"+n] = rd(t, fixHLS+n)
	}
	return newSite(t, files)
}

func decodeMP4(t *testing.T, p string) *mp4.File {
	t.Helper()
	f, err := mp4.DecodeFile(bytes.NewReader(rd(t, p)))
	if err != nil {
		t.Fatalf("o resultado não é um MP4 legível: %v", err)
	}
	return f
}

// assertPlayable valida de forma independente (ffprobe/ffmpeg) quando existem.
func assertPlayable(t *testing.T, p string) {
	t.Helper()
	f := decodeMP4(t, p)
	kinds := map[string]bool{}
	for _, tr := range f.Moov.Traks {
		kinds[tr.Mdia.Hdlr.HandlerType] = true
	}
	if len(f.Moov.Traks) != 2 || !kinds["vide"] || !kinds["soun"] {
		t.Fatalf("esperava 1 faixa de vídeo + 1 de áudio, obtido %d (%v)", len(f.Moov.Traks), kinds)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Log("ffmpeg ausente: só validado com o mp4ff")
		return
	}
	if b, err := exec.Command("ffmpeg", "-v", "error", "-i", p, "-f", "null", "-").CombinedOutput(); err != nil || len(bytes.TrimSpace(b)) > 0 {
		t.Errorf("o ffmpeg não descodifica o resultado sem erros: %v\n%s", err, b)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", p).Output()
	if err != nil || !strings.Contains(string(out), "video") || !strings.Contains(string(out), "audio") {
		t.Errorf("o ffprobe devia ver vídeo e áudio: %v %q", err, out)
	}
}

func completedInfo(t *testing.T, s *sink) (path, sha string) {
	t.Helper()
	ev := s.find(engine.EventCompleted)
	if ev == nil {
		t.Fatalf("sem EventCompleted; erro: %v", s.lastErr())
	}
	return ev.Payload["path"], ev.Payload["sha256"]
}

func TestHLSWithSeparateAudioIsDownloadedAndMergedIntoOnePlayableMP4(t *testing.T) {
	site := hlsSeparateAudioSite(t)
	dir := t.TempDir()
	e := newEngine(nil)
	defer e.Close()
	s, j := runTask(t, e, hlsTask("m1", site.URL+"/master.m3u8", dir))
	if j.err != nil {
		t.Fatalf("falhou: %v", j.err)
	}
	p, sha := completedInfo(t, s)
	if filepath.Base(p) != "video.mp4" {
		t.Errorf("o resultado junto é um .mp4, obtido %s", filepath.Base(p))
	}
	assertPlayable(t, p)
	sum := sha256.Sum256(rd(t, p))
	if sha != hex.EncodeToString(sum[:]) {
		t.Errorf("o sha256 do evento tem de ser o do ficheiro final: %s", sha)
	}
	// todos os segmentos das DUAS faixas foram pedidos
	if site.hitsMatching("/video/", ".m4s") != 3 || site.hitsMatching("/audio/", ".m4s") != 4 {
		t.Errorf("segmentos pedidos: vídeo=%d (3) áudio=%d (4)", site.hitsMatching("/video/", ".m4s"), site.hitsMatching("/audio/", ".m4s"))
	}
	snap := j.Snapshot()
	if fi, _ := os.Stat(p); snap.BytesDone != fi.Size() || snap.BytesTotal != fi.Size() {
		t.Errorf("progresso final = tamanho do ficheiro: %+v (%d)", snap, fi.Size())
	}
	noLeftovers(t, dir)
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Errorf("só devia ficar o ficheiro final: %v", es)
	}
}

// A retoma não repete a faixa já descarregada: pausa-se com o áudio bloqueado.
func TestPauseResumeBetweenTracksDoesNotRedownloadTheFinishedTrack(t *testing.T) {
	site := hlsSeparateAudioSite(t)
	gate := site.setGate(func(p string) bool { return strings.HasPrefix(p, "/audio/") && strings.HasSuffix(p, ".m4s") })
	dir := t.TempDir()
	e := newEngine(nil)
	defer e.Close()
	task := hlsTask("m2", site.URL+"/master.m3u8", dir)
	s1 := &sink{}
	h, err := e.Start(context.Background(), engine.StartRequest{Task: task, Sink: s1})
	if err != nil {
		t.Fatal(err)
	}
	j := h.(*job)

	// espera que a faixa de vídeo termine (marca Done) e o áudio fique bloqueado
	work := filepath.Join(dir, ".svhls-m2", "t0")
	deadline := time.Now().Add(15 * time.Second)
	for {
		if st := loadState(work); st != nil && st.Done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a faixa de vídeo nunca ficou concluída")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap := j.Snapshot(); snap.BytesTotal != -1 {
		t.Errorf("com faixas por descarregar o total é desconhecido (-1), obtido %d", snap.BytesTotal)
	}
	h.Pause(context.Background())
	<-j.done
	if j.err != nil || s1.count(engine.EventCompleted) != 0 {
		t.Fatalf("pausa não é erro nem conclusão: %v", j.err)
	}
	videoHits, videoInit := site.hitsMatching("/video/", ".m4s"), site.hitsMatching("/video/v-init", ".mp4")

	close(gate) // o áudio passa a responder
	s2, j2 := runTask(t, e, task)
	if j2.err != nil {
		t.Fatalf("retoma falhou: %v", j2.err)
	}
	p, _ := completedInfo(t, s2)
	assertPlayable(t, p)
	if got := site.hitsMatching("/video/", ".m4s"); got != videoHits {
		t.Errorf("a faixa de vídeo concluída foi descarregada outra vez: %d → %d pedidos de segmentos", videoHits, got)
	}
	if got := site.hitsMatching("/video/v-init", ".mp4"); got != videoInit {
		t.Errorf("o init do vídeo foi pedido outra vez: %d → %d", videoInit, got)
	}
	noLeftovers(t, dir)
}

func TestSeparateAudioInMPEGTSStaysRefusedWithoutDownloadingSegments(t *testing.T) {
	// reaproveita o fixture TS antigo (/audio-master.m3u8 → aud.m3u8 em TS)
	srv := newHLS(t, hlsOpts{nSegs: 3, segSize: 1000})
	dir := t.TempDir()
	e := newEngine(nil)
	defer e.Close()
	s, j := runTask(t, e, hlsTask("m3", srv.URL+"/audio-master.m3u8", dir))
	de := s.lastErr()
	if j.err == nil || de == nil || de.Class != domain.ErrUnsupportedSource || !strings.Contains(de.Message, "MP4 fragmentado") || de.Action == "" {
		t.Fatalf("esperava recusa explicada (só MP4 fragmentado): %v %+v", j.err, de)
	}
	if srv.totalSegHits() != 0 {
		t.Error("não deve descarregar segmentos de um stream recusado")
	}
}

func TestMergeFailureIsExplainedAndLeavesNoFinalFile(t *testing.T) {
	// o "áudio" passa a ser um fMP4 cujo moov tem 2 faixas → não se consegue juntar
	files := map[string][]byte{
		"/master.m3u8": []byte("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"en\",URI=\"audio/a.m3u8\"\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=200000,AUDIO=\"aud\"\nvideo/v.m3u8\n"),
		"/video/v.m3u8": rd(t, fixHLS+"v.m3u8"),
		"/audio/a.m3u8": []byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-MAP:URI=\"m.mp4\"\n#EXTINF:2,\nm0.m4s\n#EXT-X-ENDLIST\n"),
		"/audio/m.mp4":  rd(t, "../mp4merge/testdata/muxed.mp4"), // fMP4 com vídeo+áudio no mesmo ficheiro
		"/audio/m0.m4s": {},
	}
	for _, n := range []string{"v-init.mp4", "v0.m4s", "v1.m4s", "v2.m4s"} {
		files["/video/"+n] = rd(t, fixHLS+n)
	}
	bad := newSite(t, files)
	dir := t.TempDir()
	e := newEngine(nil)
	defer e.Close()
	s, j := runTask(t, e, hlsTask("m4", bad.URL+"/master.m3u8", dir))
	de := s.lastErr()
	if j.err == nil || de == nil || !strings.Contains(de.Message, "juntar") || de.Action == "" {
		t.Fatalf("esperava erro explicado ao juntar: %v %+v", j.err, de)
	}
	if es, _ := os.ReadDir(dir); func() bool {
		for _, x := range es {
			if !strings.HasPrefix(x.Name(), ".svhls-") {
				return true
			}
		}
		return false
	}() {
		t.Errorf("não podia ficar nenhum ficheiro final/parcial fora da pasta de trabalho: %v", es)
	}
}

// ---- DASH ----

func dashSite(t *testing.T) *siteTree {
	t.Helper()
	files := map[string][]byte{"/v/manifest.mpd": rd(t, fixDASH+"manifest.mpd")}
	es, _ := os.ReadDir(fixDASH)
	for _, e := range es {
		if strings.HasSuffix(e.Name(), ".m4s") {
			files["/v/"+e.Name()] = rd(t, fixDASH+e.Name())
		}
	}
	return newSite(t, files)
}

func dashTask(id, url, dir string) domain.DownloadTask {
	return domain.DownloadTask{ID: id, State: domain.StateRunning,
		Source: domain.DownloadSource{Kind: domain.SourceDASH, URL: url, DisplayName: "aula", Metadata: map[string]string{}},
		Policy: domain.DownloadPolicy{DestinationDir: dir, AllowResume: true}}
}

func TestDASHVideoAndAudioFromARealFFmpegManifestIsMergedAndPlayable(t *testing.T) {
	site := dashSite(t)
	dir := t.TempDir()
	e := newEngine(nil)
	defer e.Close()
	s, j := runTask(t, e, dashTask("d1", site.URL+"/v/manifest.mpd", dir))
	if j.err != nil {
		t.Fatalf("falhou: %v (%+v)", j.err, s.lastErr())
	}
	p, _ := completedInfo(t, s)
	if filepath.Base(p) != "aula.mp4" {
		t.Errorf("nome final: %s", filepath.Base(p))
	}
	assertPlayable(t, p)
	// 2 inits + 3 + 3 segmentos
	n := 0
	for path, c := range site.hits {
		if strings.HasSuffix(path, ".m4s") {
			n += c
		}
	}
	if n != 2+3+3 {
		t.Errorf("pedidos de .m4s: %d (esperados 8)", n)
	}
	noLeftovers(t, dir)
}

// Um MPD só com vídeo (áudio já dentro, ou sem áudio) não passa pelo merge.
func TestDASHSingleTrackIsJustConcatenated(t *testing.T) {
	mpd := `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT6S">
<Period><AdaptationSet contentType="video"><Representation id="0" mimeType="video/mp4" bandwidth="1000">
<SegmentTemplate timescale="12800" initialization="init-stream$RepresentationID$.m4s" media="chunk-stream$RepresentationID$-$Number%05d$.m4s" startNumber="1">
<SegmentTimeline><S t="0" d="25600" r="2"/></SegmentTimeline></SegmentTemplate></Representation></AdaptationSet></Period></MPD>`
	files := map[string][]byte{"/m.mpd": []byte(mpd)}
	var want []byte
	want = append(want, rd(t, fixDASH+"init-stream0.m4s")...)
	files["/init-stream0.m4s"] = rd(t, fixDASH+"init-stream0.m4s")
	for i := 1; i <= 3; i++ {
		n := "chunk-stream0-0000" + string(rune('0'+i)) + ".m4s"
		b := rd(t, fixDASH+n)
		files["/"+n] = b
		want = append(want, b...)
	}
	site := newSite(t, files)
	dir := t.TempDir()
	e := newEngine(nil)
	defer e.Close()
	s, j := runTask(t, e, dashTask("d2", site.URL+"/m.mpd", dir))
	if j.err != nil {
		t.Fatalf("falhou: %v (%+v)", j.err, s.lastErr())
	}
	p, _ := completedInfo(t, s)
	if !bytes.Equal(rd(t, p), want) {
		t.Error("uma só faixa é a concatenação exata de init + segmentos")
	}
	if filepath.Ext(p) != ".mp4" {
		t.Errorf("extensão: %s", p)
	}
}

func TestDASHRefusalsAreExplainedBeforeAnyDownload(t *testing.T) {
	cases := map[string]struct{ mpd, msg string }{
		"ao vivo": {`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"><Period/></MPD>`, "ao vivo"},
		"DRM": {`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT4S"><Period><AdaptationSet contentType="video">
<ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/>
<Representation id="1" mimeType="video/mp4" bandwidth="1"><SegmentTemplate media="s$Number$.m4s" duration="2" startNumber="1"/></Representation></AdaptationSet></Period></MPD>`, "protegido"},
		"vários períodos": {`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static"><Period/><Period/></MPD>`, "períodos"},
		"não é XML":       {`isto não é um manifesto`, "inválido"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			site := newSite(t, map[string][]byte{"/m.mpd": []byte(c.mpd)})
			dir := t.TempDir()
			e := newEngine(nil)
			defer e.Close()
			s, j := runTask(t, e, dashTask("dr", site.URL+"/m.mpd", dir))
			de := s.lastErr()
			if j.err == nil || de == nil || de.Class != domain.ErrUnsupportedSource || !strings.Contains(de.Message, c.msg) || de.Action == "" {
				t.Fatalf("esperava recusa explicada (%q): %v %+v", c.msg, j.err, de)
			}
			if n := site.hitsMatching("/", ".m4s"); n != 0 {
				t.Errorf("não devia descarregar segmentos: %d", n)
			}
			if es, _ := os.ReadDir(dir); len(es) != 0 {
				t.Errorf("não devia deixar ficheiros: %v", es)
			}
		})
	}
}
