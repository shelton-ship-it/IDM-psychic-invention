package lan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"streamvault/core/security"
)

type linkOffer struct {
	Link string `json:"link"`
}

type fileOffer struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (r *Receiver) onLink(sc *secureConn, peer string, p []byte) {
	var o linkOffer
	if json.Unmarshal(p, &o) != nil || o.Link == "" || len(o.Link) > 4096 {
		reject(sc, "oferta inválida")
		return
	}
	if r.o.OnLink == nil || !r.o.OnLink(peer, o.Link) {
		reject(sc, "recusado pelo utilizador")
		return
	}
	_ = sc.write(msgAccept, nil)
}

// onFile devolve false se a sessão deve terminar (erro de protocolo/IO).
func (r *Receiver) onFile(sc *secureConn, c net.Conn, peer string, p []byte) bool {
	var o fileOffer
	if json.Unmarshal(p, &o) != nil {
		reject(sc, "oferta inválida")
		return true
	}
	clean, findings := security.CheckFileName(o.Name)
	for _, f := range findings {
		if f.Severity == "block" {
			reject(sc, "nome de ficheiro inseguro")
			return true
		}
	}
	// só nomes simples: um dispositivo remoto não escolhe subpastas
	if clean == "" || strings.ContainsAny(clean, `/\`) || clean == "." {
		reject(sc, "nome de ficheiro inseguro")
		return true
	}
	if o.Size < 0 || o.Size > r.o.MaxFileSize {
		reject(sc, "tamanho acima do limite")
		return true
	}
	want, err := hex.DecodeString(o.SHA256)
	if err != nil || len(want) != 32 {
		reject(sc, "hash inválido")
		return true
	}
	// um ficheiro nunca é aceite sem confirmação explícita do utilizador
	if r.o.OnFile == nil || !r.o.OnFile(peer, clean, o.Size) {
		reject(sc, "recusado pelo utilizador")
		return true
	}
	if err := os.MkdirAll(r.o.Dir, 0o700); err != nil {
		reject(sc, "pasta de destino indisponível")
		return true
	}
	tmp, f, err := createExclusive(r.o.Dir, clean+".svpart")
	if err != nil {
		reject(sc, "não foi possível criar o ficheiro")
		return true
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(tmp)
		}
	}()
	if sc.write(msgAccept, nil) != nil {
		return false
	}
	h := sha256.New()
	var got int64
	for {
		c.SetDeadline(time.Now().Add(idleTimeout))
		typ, data, err := sc.read()
		if err != nil {
			return false
		}
		if typ == msgEnd {
			break
		}
		if typ != msgData {
			return false
		}
		got += int64(len(data))
		if got > o.Size { // mais bytes do que o declarado: abortar
			_ = sc.write(msgFail, []byte("excedeu o tamanho declarado"))
			return false
		}
		if _, err := f.Write(data); err != nil {
			_ = sc.write(msgFail, []byte("erro de escrita"))
			return false
		}
		h.Write(data)
	}
	if got != o.Size || hex.EncodeToString(h.Sum(nil)) != strings.ToLower(o.SHA256) {
		_ = sc.write(msgFail, []byte("verificação de integridade falhou"))
		return true
	}
	if err := f.Sync(); err != nil {
		_ = sc.write(msgFail, []byte("erro de escrita"))
		return true
	}
	f.Close()
	final := uniquePath(filepath.Join(r.o.Dir, clean))
	if err := os.Rename(tmp, final); err != nil {
		_ = sc.write(msgFail, []byte("não foi possível finalizar"))
		return true
	}
	ok = true
	if r.o.OnFileReceived != nil {
		r.o.OnFileReceived(peer, final)
	}
	_ = sc.write(msgDone, nil)
	return true
}

// createExclusive cria um ficheiro novo (O_EXCL: nunca abre um existente).
func createExclusive(dir, name string) (string, *os.File, error) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	ext := filepath.Ext(name)
	for i := 0; i < 1000; i++ {
		n := name
		if i > 0 {
			n = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		p := filepath.Join(dir, n)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return p, f, nil
		}
		if !os.IsExist(err) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("lan: demasiados ficheiros com o mesmo nome")
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
