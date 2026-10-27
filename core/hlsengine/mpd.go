package hlsengine

import (
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// ---- modelo mínimo do MPD (só o que o descarregador usa) ----

type mpdDoc struct {
	XMLName  xml.Name    `xml:"MPD"`
	Type     string      `xml:"type,attr"`
	Duration string      `xml:"mediaPresentationDuration,attr"`
	BaseURLs []string    `xml:"BaseURL"`
	Protect  []xmlAny    `xml:"ContentProtection"`
	Periods  []mpdPeriod `xml:"Period"`
}

type xmlAny struct {
	Scheme string `xml:"schemeIdUri,attr"`
}

type mpdPeriod struct {
	Duration string          `xml:"duration,attr"`
	BaseURLs []string        `xml:"BaseURL"`
	Sets     []mpdAdaptation `xml:"AdaptationSet"`
	Tmpl     *mpdTemplate    `xml:"SegmentTemplate"`
	List     *mpdList        `xml:"SegmentList"`
	Protect  []xmlAny        `xml:"ContentProtection"`
}

type mpdAdaptation struct {
	ContentType string       `xml:"contentType,attr"`
	MimeType    string       `xml:"mimeType,attr"`
	Lang        string       `xml:"lang,attr"`
	BaseURLs    []string     `xml:"BaseURL"`
	Roles       []mpdRole    `xml:"Role"`
	Protect     []xmlAny     `xml:"ContentProtection"`
	Tmpl        *mpdTemplate `xml:"SegmentTemplate"`
	List        *mpdList     `xml:"SegmentList"`
	Reps        []mpdRep     `xml:"Representation"`
}

type mpdRole struct {
	Value string `xml:"value,attr"`
}

type mpdRep struct {
	ID        string       `xml:"id,attr"`
	MimeType  string       `xml:"mimeType,attr"`
	Bandwidth int64        `xml:"bandwidth,attr"`
	Height    int          `xml:"height,attr"`
	BaseURLs  []string     `xml:"BaseURL"`
	Protect   []xmlAny     `xml:"ContentProtection"`
	Tmpl      *mpdTemplate `xml:"SegmentTemplate"`
	List      *mpdList     `xml:"SegmentList"`
	Base      *mpdBase     `xml:"SegmentBase"`
}

type mpdTemplate struct {
	Timescale   uint64       `xml:"timescale,attr"`
	Duration    uint64       `xml:"duration,attr"`
	StartNumber *int64       `xml:"startNumber,attr"`
	Init        string       `xml:"initialization,attr"`
	Media       string       `xml:"media,attr"`
	InitEl      *mpdURL      `xml:"Initialization"`
	Timeline    *mpdTimeline `xml:"SegmentTimeline"`
}

type mpdTimeline struct {
	S []mpdS `xml:"S"`
}

type mpdS struct {
	T *uint64 `xml:"t,attr"`
	D uint64  `xml:"d,attr"`
	R int64   `xml:"r,attr"`
}

type mpdList struct {
	Timescale uint64    `xml:"timescale,attr"`
	Duration  uint64    `xml:"duration,attr"`
	InitEl    *mpdURL   `xml:"Initialization"`
	URLs      []mpdSegU `xml:"SegmentURL"`
}

type mpdURL struct {
	SourceURL string `xml:"sourceURL,attr"`
	Range     string `xml:"range,attr"`
}

type mpdSegU struct {
	Media      string `xml:"media,attr"`
	MediaRange string `xml:"mediaRange,attr"`
}

type mpdBase struct {
	InitEl *mpdURL `xml:"Initialization"`
}

// ---- análise ----

// ParseMPD interpreta um MPD estático (VOD) sem DRM e devolve as faixas a
// descarregar: a de vídeo de maior débito e a primeira de áudio principal, na
// ordem [vídeo, áudio] (só as que existirem). Recusa, com explicação: transmissões
// ao vivo (type="dynamic"), vários períodos, qualquer ContentProtection (DRM/cifra),
// intervalos de bytes e contentores que não sejam MP4 ou WebM.
func ParseMPD(text string, base *url.URL) ([]*Playlist, error) {
	if len(text) > maxPlaylistBytes {
		return nil, errors.New("MPD demasiado grande")
	}
	var doc mpdDoc
	dec := xml.NewDecoder(strings.NewReader(text))
	dec.Strict = true
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("MPD inválido: %w", err)
	}
	if strings.EqualFold(doc.Type, "dynamic") {
		return nil, unsupported("É uma transmissão ao vivo (MPD dinâmico).", "Só vídeos em demanda (VOD) são suportados.")
	}
	if len(doc.Periods) == 0 {
		return nil, errors.New("o MPD não tem períodos")
	}
	if len(doc.Periods) > 1 {
		return nil, unsupported("O MPD tem vários períodos (típico de publicidade inserida).", "Ainda não suportado.")
	}
	p := doc.Periods[0]
	if drm(doc.Protect) || drm(p.Protect) {
		return nil, drmError()
	}

	total := 0.0
	for _, d := range []string{p.Duration, doc.Duration} {
		if d != "" {
			v, err := parseISODuration(d)
			if err != nil {
				return nil, fmt.Errorf("duração inválida %q: %w", truncate(d, 40), err)
			}
			total = v
			break
		}
	}

	root, err := chainBase(base, doc.BaseURLs, p.BaseURLs)
	if err != nil {
		return nil, err
	}

	var videoSet, audioSet *mpdAdaptation
	var videoRep, audioRep *mpdRep
	for i := range p.Sets {
		as := &p.Sets[i]
		if drm(as.Protect) {
			return nil, drmError()
		}
		kind := adaptationKind(as)
		for k := range as.Reps {
			r := &as.Reps[k]
			if drm(r.Protect) {
				return nil, drmError()
			}
			switch kind {
			case "video":
				if videoRep == nil || r.Bandwidth > videoRep.Bandwidth || (r.Bandwidth == videoRep.Bandwidth && r.Height > videoRep.Height) {
					videoSet, videoRep = as, r
				}
			case "audio":
				// preferir o áudio "main"; a 1.ª faixa de áudio na falta dele
				if audioSet != nil && audioSet != as {
					if isMain(audioSet) || !isMain(as) {
						continue
					}
					audioSet, audioRep = nil, nil
				}
				if audioRep == nil || r.Bandwidth > audioRep.Bandwidth {
					audioSet, audioRep = as, r
				}
			}
		}
	}
	if videoRep == nil && audioRep == nil {
		return nil, unsupported("O MPD não tem faixas de vídeo nem de áudio suportadas.", "Confirme o link.")
	}

	var out []*Playlist
	for _, c := range []struct {
		set  *mpdAdaptation
		rep  *mpdRep
		kind string
	}{{videoSet, videoRep, "video"}, {audioSet, audioRep, "audio"}} {
		if c.rep == nil {
			continue
		}
		pl, err := buildTrack(p, c.set, c.rep, root, total, c.kind)
		if err != nil {
			return nil, err
		}
		out = append(out, pl)
	}
	return out, nil
}

func drm(l []xmlAny) bool { return len(l) > 0 }

func drmError() error {
	return unsupported("O stream está protegido (DRM/cifra: ContentProtection). O StreamVault não descarrega conteúdo protegido.",
		"Se o conteúdo for seu, peça o ficheiro original a quem o publicou.")
}

func adaptationKind(as *mpdAdaptation) string {
	ct := strings.ToLower(as.ContentType)
	if ct == "" {
		m := strings.ToLower(as.MimeType)
		if m == "" && len(as.Reps) > 0 {
			m = strings.ToLower(as.Reps[0].MimeType)
		}
		ct, _, _ = strings.Cut(m, "/")
	}
	switch ct {
	case "video", "audio":
		return ct
	}
	return ""
}

func isMain(as *mpdAdaptation) bool {
	for _, r := range as.Roles {
		if strings.EqualFold(r.Value, "main") {
			return true
		}
	}
	return false
}

// chainBase aplica as cadeias de <BaseURL> (a primeira de cada nível, relativa ao nível anterior).
func chainBase(base *url.URL, levels ...[]string) (*url.URL, error) {
	cur := base
	for _, l := range levels {
		if len(l) == 0 || strings.TrimSpace(l[0]) == "" {
			continue
		}
		ref := strings.TrimSpace(l[0])
		if len(ref) > 4096 {
			return nil, errors.New("BaseURL demasiado longo")
		}
		nu, err := cur.Parse(ref)
		if err != nil || (nu.Scheme != "http" && nu.Scheme != "https") || nu.Host == "" {
			return nil, fmt.Errorf("BaseURL não suportado: %q", truncate(ref, 60))
		}
		cur = nu
	}
	return cur, nil
}

var fmtSpec = regexp.MustCompile(`^%0([0-9]{1,2})d$`)

// expand aplica as substituições $Identificador[%0Nd]$ de um modelo DASH.
func expand(tmpl, repID string, bandwidth int64, number int64, tm uint64) (string, error) {
	var b strings.Builder
	for i := 0; i < len(tmpl); {
		c := tmpl[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		j := strings.IndexByte(tmpl[i+1:], '$')
		if j < 0 {
			return "", errors.New("modelo com '$' por fechar")
		}
		tok := tmpl[i+1 : i+1+j]
		i += j + 2
		if tok == "" { // "$$" = '$' literal
			b.WriteByte('$')
			continue
		}
		name, spec, _ := strings.Cut(tok, "%")
		width := 1
		if spec != "" {
			m := fmtSpec.FindStringSubmatch("%" + spec)
			if m == nil {
				return "", fmt.Errorf("formato %q não suportado", truncate(tok, 30))
			}
			width, _ = strconv.Atoi(m[1])
		}
		var val string
		switch name {
		case "RepresentationID":
			val = repID
		case "Number":
			val = fmt.Sprintf("%0*d", width, number)
		case "Time":
			val = fmt.Sprintf("%0*d", width, tm)
		case "Bandwidth":
			val = fmt.Sprintf("%0*d", width, bandwidth)
		default:
			return "", fmt.Errorf("identificador $%s$ não suportado", truncate(name, 30))
		}
		b.WriteString(val)
	}
	return b.String(), nil
}

func buildTrack(p mpdPeriod, as *mpdAdaptation, r *mpdRep, root *url.URL, total float64, kind string) (*Playlist, error) {
	base, err := chainBase(root, as.BaseURLs, r.BaseURLs)
	if err != nil {
		return nil, err
	}
	mime := strings.ToLower(r.MimeType)
	if mime == "" {
		mime = strings.ToLower(as.MimeType)
	}
	resolveRef := func(ref string) (string, error) {
		if len(ref) > 4096 {
			return "", errors.New("URL demasiado longo")
		}
		u, err := base.Parse(ref)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", fmt.Errorf("URL de segmento não suportado: %q", truncate(ref, 60))
		}
		return u.String(), nil
	}

	pl := &Playlist{EndList: true}
	tmpl := mergeTemplate(p.Tmpl, as.Tmpl, r.Tmpl)
	list := firstList(r.List, as.List, p.List)

	switch {
	case tmpl != nil && tmpl.Media != "":
		start := int64(1)
		if tmpl.StartNumber != nil {
			start = *tmpl.StartNumber
		}
		ts := tmpl.Timescale
		if ts == 0 {
			ts = 1
		}
		initRef := tmpl.Init
		if initRef == "" && tmpl.InitEl != nil {
			initRef = tmpl.InitEl.SourceURL
		}
		if tmpl.InitEl != nil && tmpl.InitEl.Range != "" {
			return nil, unsupported("O stream usa intervalos de bytes no segmento de inicialização.", "Ainda não suportado.")
		}
		if initRef != "" {
			s, err := expand(initRef, r.ID, r.Bandwidth, start, 0)
			if err != nil {
				return nil, err
			}
			if pl.MapURL, err = resolveRef(s); err != nil {
				return nil, err
			}
		}
		type seg struct {
			num int64
			t   uint64
			d   uint64
		}
		var segs []seg
		switch {
		case tmpl.Timeline != nil && len(tmpl.Timeline.S) > 0:
			var cur uint64
			num := start
			for i, s := range tmpl.Timeline.S {
				if s.T != nil {
					cur = *s.T
				}
				if s.D == 0 {
					return nil, errors.New("SegmentTimeline com duração 0")
				}
				rep := s.R
				if rep < 0 { // repete até ao fim do período
					if total <= 0 {
						return nil, errors.New("SegmentTimeline com r negativo exige a duração do período")
					}
					end := total * float64(ts)
					if i+1 < len(tmpl.Timeline.S) && tmpl.Timeline.S[i+1].T != nil {
						end = float64(*tmpl.Timeline.S[i+1].T)
					}
					rep = int64(math.Ceil((end-float64(cur))/float64(s.D))) - 1
					if rep < 0 {
						rep = 0
					}
				}
				for k := int64(0); k <= rep; k++ {
					if len(segs) >= maxSegments {
						return nil, fmt.Errorf("mais de %d segmentos", maxSegments)
					}
					segs = append(segs, seg{num, cur, s.D})
					cur += s.D
					num++
				}
			}
		case tmpl.Duration > 0:
			if total <= 0 {
				return nil, errors.New("o MPD não indica a duração (mediaPresentationDuration)")
			}
			dur := float64(tmpl.Duration) / float64(ts)
			n := int64(math.Ceil(total / dur))
			if n <= 0 || n > maxSegments {
				return nil, fmt.Errorf("número de segmentos inválido (%d)", n)
			}
			for k := int64(0); k < n; k++ {
				segs = append(segs, seg{start + k, uint64(k) * tmpl.Duration, tmpl.Duration})
			}
		default:
			return nil, errors.New("SegmentTemplate sem duração nem SegmentTimeline")
		}
		for _, s := range segs {
			ref, err := expand(tmpl.Media, r.ID, r.Bandwidth, s.num, s.t)
			if err != nil {
				return nil, err
			}
			u, err := resolveRef(ref)
			if err != nil {
				return nil, err
			}
			pl.Segments = append(pl.Segments, Segment{URL: u, Duration: float64(s.d) / float64(ts)})
		}

	case list != nil && len(list.URLs) > 0:
		ts := list.Timescale
		if ts == 0 {
			ts = 1
		}
		if list.InitEl != nil {
			if list.InitEl.Range != "" {
				return nil, unsupported("O stream usa intervalos de bytes no segmento de inicialização.", "Ainda não suportado.")
			}
			if list.InitEl.SourceURL != "" {
				if pl.MapURL, err = resolveRef(list.InitEl.SourceURL); err != nil {
					return nil, err
				}
			}
		}
		if len(list.URLs) > maxSegments {
			return nil, fmt.Errorf("mais de %d segmentos", maxSegments)
		}
		for _, su := range list.URLs {
			if su.MediaRange != "" {
				return nil, unsupported("O stream usa intervalos de bytes dentro de ficheiros.", "Ainda não suportado.")
			}
			if su.Media == "" {
				return nil, errors.New("SegmentURL sem media")
			}
			u, err := resolveRef(su.Media)
			if err != nil {
				return nil, err
			}
			pl.Segments = append(pl.Segments, Segment{URL: u, Duration: float64(list.Duration) / float64(ts)})
		}

	default:
		// ficheiro único (perfil on-demand): a própria BaseURL da representação
		if len(r.BaseURLs) == 0 && len(as.BaseURLs) == 0 {
			return nil, unsupported("A representação não indica onde estão os segmentos.", "Este stream não é suportado.")
		}
		pl.Segments = []Segment{{URL: base.String()}}
	}
	if len(pl.Segments) == 0 {
		return nil, errors.New("a representação não tem segmentos")
	}

	// contentor
	switch {
	case strings.HasSuffix(mime, "/mp4"):
		pl.Container = "mp4"
	case strings.HasSuffix(mime, "/webm"):
		pl.Container = "webm"
	case mime == "":
		switch strings.ToLower(path.Ext(mustPath(pl.Segments[0].URL))) {
		case ".mp4", ".m4s", ".m4v", ".m4a", ".cmfv", ".cmfa":
			pl.Container = "mp4"
		case ".webm", ".weba":
			pl.Container = "webm"
		}
	}
	if pl.Container == "" {
		return nil, unsupported("Formato de contentor não suportado ("+truncate(mime, 40)+").", "Só MP4 e WebM são suportados em DASH.")
	}
	switch {
	case pl.Container == "webm":
		pl.Ext = ".webm"
	case kind == "audio":
		pl.Ext = ".m4a"
	default:
		pl.Ext = ".mp4"
	}
	return pl, nil
}

// mergeTemplate: os atributos mais próximos da representação prevalecem.
func mergeTemplate(ts ...*mpdTemplate) *mpdTemplate {
	var out *mpdTemplate
	for _, t := range ts { // período → adaptação → representação
		if t == nil {
			continue
		}
		if out == nil {
			c := *t
			out = &c
			continue
		}
		if t.Timescale != 0 {
			out.Timescale = t.Timescale
		}
		if t.Duration != 0 {
			out.Duration = t.Duration
		}
		if t.StartNumber != nil {
			out.StartNumber = t.StartNumber
		}
		if t.Init != "" {
			out.Init = t.Init
		}
		if t.Media != "" {
			out.Media = t.Media
		}
		if t.InitEl != nil {
			out.InitEl = t.InitEl
		}
		if t.Timeline != nil {
			out.Timeline = t.Timeline
		}
	}
	return out
}

func firstList(ls ...*mpdList) *mpdList {
	for _, l := range ls {
		if l != nil {
			return l
		}
	}
	return nil
}

var isoDur = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

// parseISODuration converte "PT1H2M3.5S" / "P1DT2H" em segundos (sem anos/meses).
func parseISODuration(s string) (float64, error) {
	m := isoDur.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil || s == "P" || strings.HasSuffix(s, "T") {
		return 0, errors.New("formato ISO 8601 não suportado")
	}
	num := func(x string) float64 { v, _ := strconv.ParseFloat(x, 64); return v }
	v := num(m[1])*86400 + num(m[2])*3600 + num(m[3])*60 + num(m[4])
	if v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, errors.New("duração inválida")
	}
	return v, nil
}
