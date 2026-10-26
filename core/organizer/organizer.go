// Package organizer implementa a Fase 9: categorização de ficheiros
// concluídos, destino seguro (sem sobrescrever, sem sair da pasta base) e
// deteção de duplicados EXATOS por SHA-256. Nunca apaga nada sozinho: remover
// um duplicado é uma decisão do utilizador.
package organizer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Categorias estáveis (nomes de pasta e chaves para a UI).
const (
	CatVideo    = "Videos"
	CatAudio    = "Musica"
	CatImage    = "Imagens"
	CatDocument = "Documentos"
	CatArchive  = "Arquivos"
	CatSoftware = "Programas"
	CatTorrent  = "Torrents"
	CatOther    = "Outros"
)

var extCategory = map[string]string{
	// vídeo
	".mp4": CatVideo, ".mkv": CatVideo, ".avi": CatVideo, ".mov": CatVideo, ".webm": CatVideo, ".m4v": CatVideo, ".wmv": CatVideo,
	// áudio
	".mp3": CatAudio, ".flac": CatAudio, ".wav": CatAudio, ".m4a": CatAudio, ".ogg": CatAudio, ".opus": CatAudio, ".aac": CatAudio,
	// imagem
	".jpg": CatImage, ".jpeg": CatImage, ".png": CatImage, ".gif": CatImage, ".webp": CatImage, ".bmp": CatImage, ".svg": CatImage, ".heic": CatImage,
	// documento
	".pdf": CatDocument, ".doc": CatDocument, ".docx": CatDocument, ".xls": CatDocument, ".xlsx": CatDocument,
	".ppt": CatDocument, ".pptx": CatDocument, ".txt": CatDocument, ".md": CatDocument, ".epub": CatDocument, ".csv": CatDocument,
	// arquivos
	".zip": CatArchive, ".rar": CatArchive, ".7z": CatArchive, ".tar": CatArchive, ".gz": CatArchive, ".xz": CatArchive, ".iso": CatArchive,
	// programas (a política de segurança trata de bloquear/avisar; aqui só se classifica)
	".exe": CatSoftware, ".msi": CatSoftware, ".apk": CatSoftware, ".dmg": CatSoftware, ".deb": CatSoftware,
	// torrent
	".torrent": CatTorrent,
}

// Categorize devolve a categoria a partir do nome (e, como reserva, do
// Content-Type). Determinístico e sem rede; desconhecido => CatOther.
func Categorize(name, contentType string) string {
	if c, ok := extCategory[strings.ToLower(filepath.Ext(name))]; ok {
		return c
	}
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch {
	case strings.HasPrefix(ct, "video/"):
		return CatVideo
	case strings.HasPrefix(ct, "audio/"):
		return CatAudio
	case strings.HasPrefix(ct, "image/"):
		return CatImage
	case ct == "application/pdf":
		return CatDocument
	case ct == "application/zip", ct == "application/x-7z-compressed", ct == "application/gzip":
		return CatArchive
	case ct == "application/x-bittorrent":
		return CatTorrent
	}
	return CatOther
}

// Known descreve um ficheiro já conhecido com o mesmo hash (do índice F4).
type Known struct {
	Path      string
	SizeBytes int64
}

// Lookup consulta o índice de hashes. Implementado pelo núcleo sobre o storage.
type Lookup func(sha256Hex string) ([]Known, error)

// Action é o que o organizador fez.
type Action string

const (
	ActionMoved     Action = "moved"     // movido para a pasta da categoria
	ActionDuplicate Action = "duplicate" // existe cópia idêntica; NADA foi movido
)

// Result descreve o desfecho de forma auditável.
type Result struct {
	Action      Action
	Category    string
	NewPath     string  // preenchido quando Moved
	DuplicateOf []Known // cópias idênticas que AINDA existem em disco
}

// Options de organização.
type Options struct {
	BaseDir string // raiz sob a qual as categorias são criadas (obrigatório)
	Lookup  Lookup // opcional; nil => sem deteção de duplicados
	// MoveDuplicates: false (defeito) => se houver duplicado, não move nem apaga;
	// devolve ActionDuplicate para a UI perguntar ao utilizador.
	MoveDuplicates bool
}

// Organize classifica e move o ficheiro concluído para BaseDir/<Categoria>/.
// sha256Hex pode ser "" (sem deteção de duplicados). Garantias:
//   - nunca sobrescreve (nome único com sufixo " (n)");
//   - nunca escreve fora de BaseDir (nome limpo contra traversal);
//   - nunca apaga o original: só rename atómico.
func Organize(path, contentType, sha256Hex string, o Options) (Result, error) {
	if o.BaseDir == "" {
		return Result{}, errors.New("organizer: BaseDir obrigatório")
	}
	st, err := os.Stat(path)
	if err != nil {
		return Result{}, fmt.Errorf("organizer: ficheiro inacessível: %w", err)
	}
	if st.IsDir() {
		return Result{}, errors.New("organizer: só ficheiros (não pastas)")
	}
	name := filepath.Base(path)
	cat := Categorize(name, contentType)
	res := Result{Category: cat}

	if sha256Hex != "" && o.Lookup != nil {
		known, err := o.Lookup(strings.ToLower(sha256Hex))
		if err != nil {
			return Result{}, fmt.Errorf("organizer: índice de hashes: %w", err)
		}
		for _, k := range known {
			if sameFile(k.Path, path) {
				continue // o próprio ficheiro
			}
			if fi, err := os.Stat(k.Path); err == nil && fi.Size() == st.Size() {
				res.DuplicateOf = append(res.DuplicateOf, Known{Path: k.Path, SizeBytes: fi.Size()})
			}
		}
		if len(res.DuplicateOf) > 0 && !o.MoveDuplicates {
			res.Action = ActionDuplicate
			return res, nil
		}
	}

	destDir := filepath.Join(o.BaseDir, cat)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return Result{}, err
	}
	dest := filepath.Join(destDir, name)
	// defesa em profundidade: o destino TEM de ficar dentro de BaseDir/cat
	rel, err := filepath.Rel(destDir, dest)
	if err != nil || strings.HasPrefix(rel, "..") || strings.ContainsRune(rel, filepath.Separator) {
		return Result{}, errors.New("organizer: destino fora da pasta base recusado")
	}
	if sameFile(dest, path) {
		res.Action, res.NewPath = ActionMoved, path // já está no sítio certo
		return res, nil
	}
	dest = uniquePath(dest)
	if err := os.Rename(path, dest); err != nil {
		return Result{}, fmt.Errorf("organizer: mover: %w", err)
	}
	res.Action, res.NewPath = ActionMoved, dest
	return res, nil
}

func sameFile(a, b string) bool {
	fa, e1 := os.Stat(a)
	fb, e2 := os.Stat(b)
	return e1 == nil && e2 == nil && os.SameFile(fa, fb)
}

func uniquePath(p string) string {
	if _, err := os.Lstat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i < 10000; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Lstat(c); os.IsNotExist(err) {
			return c
		}
	}
	return fmt.Sprintf("%s (%d)%s", base, os.Getpid(), ext)
}
