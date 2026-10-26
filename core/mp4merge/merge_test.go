package mp4merge

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Fixtures gerados com o ffmpeg (HLS fMP4 de vídeo e de áudio, 6 s): ver testdata/.
const td = "testdata/hls/"

// track concatena init + segmentos (como o motor faz ao descarregar uma faixa).
func track(t *testing.T, dir, name string, parts ...string) string {
	t.Helper()
	var b bytes.Buffer
	for _, p := range parts {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
	}
	out := filepath.Join(dir, name)
	if err := os.WriteFile(out, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

func videoTrack(t *testing.T, dir string) string {
	return track(t, dir, "video.mp4", td+"v-init.mp4", td+"v0.m4s", td+"v1.m4s", td+"v2.m4s")
}

func audioTrack(t *testing.T, dir string) string {
	return track(t, dir, "audio.mp4", td+"a-init.mp4", td+"a0.m4s", td+"a1.m4s", td+"a2.m4s", td+"a3.m4s")
}

// fragInfo é o que se lê de um ficheiro (independentemente do Merge).
type fragInfo struct {
	trackID uint32
	seq     uint32
	t       float64
	payload []byte
	samples uint32
}

func readFile(t *testing.T, path string) (*mp4.File, []fragInfo) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.DecodeFile(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("o resultado não é um MP4 válido para o mp4ff: %v", err)
	}
	ts := map[uint32]uint32{}
	for _, tr := range f.Moov.Traks {
		ts[tr.Tkhd.TrackID] = tr.Mdia.Mdhd.Timescale
	}
	var out []fragInfo
	for _, seg := range f.Segments {
		for _, fr := range seg.Fragments {
			tf := fr.Moof.Traf
			fi := fragInfo{trackID: tf.Tfhd.TrackID, seq: fr.Moof.Mfhd.SequenceNumber,
				t: float64(tf.Tfdt.BaseMediaDecodeTime()) / float64(ts[tf.Tfhd.TrackID])}
			for _, tr := range tf.Truns {
				fi.samples += tr.SampleCount()
			}
			if fr.Mdat != nil {
				fi.payload = append([]byte(nil), fr.Mdat.Data...)
			}
			out = append(out, fi)
		}
	}
	return f, out
}

func TestMergeVideoAndAudioKeepsEverySampleAndInterleavesByTime(t *testing.T) {
	dir := t.TempDir()
	v, a := videoTrack(t, dir), audioTrack(t, dir)
	out := filepath.Join(dir, "out.mp4")
	st, err := MergeFiles(out, []string{v, a})
	if err != nil {
		t.Fatal(err)
	}

	_, vsrc := readFile(t, v)
	_, asrc := readFile(t, a)
	f, got := readFile(t, out)
	if st.Tracks != 2 || st.Fragments != len(vsrc)+len(asrc) || len(got) != st.Fragments {
		t.Fatalf("stats %+v; fragmentos: vídeo %d + áudio %d = %d, obtidos %d", st, len(vsrc), len(asrc), len(vsrc)+len(asrc), len(got))
	}
	if st.Bytes <= 0 {
		t.Error("devia contar os bytes escritos")
	}

	// duas faixas, ids 1 e 2, de tipos diferentes
	if n := len(f.Moov.Traks); n != 2 {
		t.Fatalf("esperava 2 faixas, obtidas %d", n)
	}
	handlers := map[uint32]string{}
	for _, tr := range f.Moov.Traks {
		handlers[tr.Tkhd.TrackID] = tr.Mdia.Hdlr.HandlerType
	}
	if handlers[1] != "vide" || handlers[2] != "soun" {
		t.Errorf("faixas: %v (esperado 1=vide 2=soun)", handlers)
	}
	if len(f.Moov.Mvex.Trexs) != 2 || f.Moov.Mvex.Trexs[0].TrackID != 1 || f.Moov.Mvex.Trexs[1].TrackID != 2 {
		t.Errorf("trex: devia haver um por faixa com os ids novos: %+v", f.Moov.Mvex.Trexs)
	}
	if f.Moov.Mvex.Mehd != nil {
		t.Error("o mehd (duração só de uma faixa) devia ter sido removido")
	}

	// números de sequência 1..N; tempo global não decrescente; por faixa estritamente crescente
	last, lastPer := -1.0, map[uint32]float64{}
	for i, g := range got {
		if g.seq != uint32(i+1) {
			t.Fatalf("sequência do fragmento %d = %d (esperado %d)", i, g.seq, i+1)
		}
		if g.t+1e-9 < last {
			t.Errorf("fragmento %d recua no tempo: %.3f < %.3f", i, g.t, last)
		}
		last = g.t
		if p, ok := lastPer[g.trackID]; ok && g.t <= p {
			t.Errorf("faixa %d não cresce: %.3f depois de %.3f", g.trackID, g.t, p)
		}
		lastPer[g.trackID] = g.t
	}
	// há mesmo intercalação (não "todo o vídeo e depois todo o áudio")
	ids := ""
	for _, g := range got {
		ids += strconv.Itoa(int(g.trackID))
	}
	if !strings.Contains(ids, "12") || !strings.Contains(ids, "21") {
		t.Errorf("fragmentos não intercalados: %s", ids)
	}

	// nada se perdeu nem foi alterado: amostras idênticas, faixa a faixa
	per := func(frags []fragInfo, id uint32) (b []byte, n uint32) {
		for _, g := range frags {
			if id == 0 || g.trackID == id {
				b = append(b, g.payload...)
				n += g.samples
			}
		}
		return
	}
	vb, vn := per(vsrc, 0)
	ab, an := per(asrc, 0)
	gvb, gvn := per(got, 1)
	gab, gan := per(got, 2)
	if !bytes.Equal(vb, gvb) || vn != gvn {
		t.Errorf("amostras de vídeo diferem (bytes %d/%d, amostras %d/%d)", len(gvb), len(vb), gvn, vn)
	}
	if !bytes.Equal(ab, gab) || an != gan {
		t.Errorf("amostras de áudio diferem (bytes %d/%d, amostras %d/%d)", len(gab), len(ab), gan, an)
	}
}

func TestMergeOrderDefinesTrackIDs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	if _, err := MergeFiles(out, []string{audioTrack(t, dir), videoTrack(t, dir)}); err != nil {
		t.Fatal(err)
	}
	f, _ := readFile(t, out)
	h := map[uint32]string{}
	for _, tr := range f.Moov.Traks {
		h[tr.Tkhd.TrackID] = tr.Mdia.Hdlr.HandlerType
	}
	if h[1] != "soun" || h[2] != "vide" {
		t.Errorf("a ordem dada define os ids: %v", h)
	}
}

// Verificação INDEPENDENTE do mp4ff: o ffprobe/ffmpeg têm de ler, descodificar e
// medir o resultado. Salta se não estiverem instalados.
func TestMergedFileIsPlayableAccordingToFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe não instalado")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out.mp4")
	if _, err := MergeFiles(out, []string{videoTrack(t, dir), audioTrack(t, dir)}); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type,codec_name,nb_read_frames,duration",
		"-count_frames", "-of", "json", out).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, raw)
	}
	var pr struct {
		Streams []struct {
			Type, Codec, Frames, Duration string
		}
	}
	// o ffprobe usa nomes em minúsculas com sublinhado: mapear à mão
	var generic struct {
		Streams []map[string]string `json:"streams"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("saída do ffprobe: %v\n%s", err, raw)
	}
	_ = pr
	kinds := []string{}
	for _, s := range generic.Streams {
		kinds = append(kinds, s["codec_type"]+":"+s["codec_name"])
		d, _ := strconv.ParseFloat(s["duration"], 64)
		if d < 5.5 || d > 6.5 {
			t.Errorf("%s: duração %v s (esperada ~6)", s["codec_type"], s["duration"])
		}
		if n, _ := strconv.Atoi(s["nb_read_frames"]); n < 100 {
			t.Errorf("%s: só %v frames descodificados", s["codec_type"], s["nb_read_frames"])
		}
	}
	sort.Strings(kinds)
	if strings.Join(kinds, ",") != "audio:aac,video:h264" {
		t.Errorf("faixas segundo o ffprobe: %v", kinds)
	}
	// descodificar tudo não pode produzir erros nem avisos
	if b, err := exec.Command("ffmpeg", "-v", "error", "-i", out, "-f", "null", "-").CombinedOutput(); err != nil || len(bytes.TrimSpace(b)) > 0 {
		t.Errorf("o ffmpeg reportou problemas ao descodificar: %v\n%s", err, b)
	}
}

func TestMergeRefusesWhatItCannotMergeWithExplanation(t *testing.T) {
	dir := t.TempDir()
	v := videoTrack(t, dir)
	cases := map[string]struct {
		srcs []string
		want string
	}{
		"não fragmentado":      {[]string{v, "testdata/plain.mp4"}, "fragmentado"},
		"várias faixas":        {[]string{v, "testdata/muxed.mp4"}, "faixas"},
		"inexistente":          {[]string{v, filepath.Join(dir, "nao-existe.mp4")}, ""},
		"só fragmentos":        {[]string{v, track(t, dir, "frag.mp4", td+"a0.m4s", td+"a1.m4s")}, "inicialização"},
		"lixo":                 {[]string{v, track(t, dir, "lixo.mp4", "testdata/hls/v.m3u8")}, ""},
		"vazio":                {[]string{v, track(t, dir, "vazio.mp4")}, "moov"},
		"truncado (init)":      {[]string{v, truncated(t, dir, td+"a-init.mp4", 300)}, ""},
		"truncado (fragmento)": {[]string{v, truncated(t, dir, audioTrack(t, dir), 3000)}, ""},
	}
	for name, tc := range cases {
		var buf bytes.Buffer
		_, err := Merge(&buf, tc.srcs)
		if err == nil {
			t.Errorf("%s: devia falhar", name)
			continue
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: erro %q não menciona %q", name, err, tc.want)
		}
		if name == "inexistente" && !errors.Is(err, os.ErrNotExist) { // independente do idioma/SO
			t.Errorf("%s: devia ser os.ErrNotExist: %v", name, err)
		}
	}
	if _, err := Merge(&bytes.Buffer{}, nil); err == nil {
		t.Error("sem ficheiros devia falhar")
	}
}

func truncated(t *testing.T, dir, src string, n int) string {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if n > len(raw) {
		n = len(raw) / 2
	}
	p := filepath.Join(dir, "trunc-"+strconv.Itoa(n)+".mp4")
	if err := os.WriteFile(p, raw[:n], 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
