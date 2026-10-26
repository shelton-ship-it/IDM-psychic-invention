// Package version expõe a versão do build. Preenchida em tempo de compilação:
//
//	go build -ldflags "-X streamvault/core/version.Version=1.0.0 -X streamvault/core/version.Commit=abc123"
package version

var (
	Version = "dev"
	Commit  = "unknown"
)
