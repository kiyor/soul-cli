// Package provider holds shared types and runtime glue for the
// model-provider proxy layer.
//
// Each concrete provider lives in a subpackage:
//   - pkg/provider/openai — Codex-protocol (OpenAI Responses API) translation
//   - pkg/provider/gemini — Google Code Assist passthrough + translation
//   - pkg/provider/ollama — Anthropic→OpenAI chat completions translation
//
// The subpackages share this Config struct, the AppName label for log
// lines, and the LoadServerAddr hook that lets them delegate proxy hosting
// to a long-running soul-cli server when one is available.
package provider

import (
	"os"
	"path/filepath"
	"strings"
)

// Config is the provider entry read from soul-cli's config.json
// (data/config.json, under "providers.<name>").
//
// Not every field applies to every provider — for example, AuthFile is only
// meaningful for the "openai" type (which uses a Codex-style auth.json on
// disk), and ChatURL is only meaningful for OpenAI-compatible chat endpoints.
type Config struct {
	BaseURL        string   `json:"baseUrl"`
	APIKey         string   `json:"apiKey"`
	AuthEnv        string   `json:"authEnv"`        // env var name for the key, default "ANTHROPIC_AUTH_TOKEN"; use "ANTHROPIC_API_KEY" for MiniMax etc.
	Models         []string `json:"models"`         // available model names for this provider
	Type           string   `json:"type"`           // "" or "anthropic" (default, passthrough) or "openai"/"gemini"/"ollama" (needs embedded translation proxy)
	ChatURL        string   `json:"chatUrl"`        // OpenAI-compatible chat completions endpoint (used when Type=="openai")
	AuthFile       string   `json:"authFile"`       // path to auth JSON (e.g. ~/.codex/auth.json, used when Type=="openai")
	SmallFastModel string   `json:"smallFastModel"` // override ANTHROPIC_SMALL_FAST_MODEL for this provider (e.g. "glm-5-turbo")
}

// ServerAddr describes where the local soul-cli server is listening.
// Empty Token means "no server is running / not configured"; subpackages
// treat that as "do not delegate, run an embedded proxy instead".
type ServerAddr struct {
	Token string
	Host  string
	Port  int
}

// AppName is the soul-cli instance label (e.g. "weiran", "hengzhun") used as
// the prefix in stderr log lines emitted by the provider subpackages. Main
// sets this once at startup; the default is only used in unit tests.
var AppName = "soul-cli"

// LoadServerAddr returns the local soul-cli server's bind address and auth
// token, or a zero-value ServerAddr when no server is running. Main wires
// this to its own loadServerConfig() at startup. The default is a stub that
// keeps unit tests (which never delegate to a server) from panicking.
var LoadServerAddr = func() ServerAddr { return ServerAddr{} }

// ExpandPath expands a leading ~ in p to the user's home directory. Used by
// subpackages to resolve AuthFile paths from Config. Returns p unchanged if
// it doesn't start with ~ or if $HOME cannot be determined.
func ExpandPath(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}

// ActivePort is non-zero when an embedded provider proxy (openai, gemini,
// or ollama) is running in the current process. Main's claude launcher
// reads this to decide whether syscall.Exec is safe: exec replaces the
// entire process image, which would kill the proxy goroutine before Claude
// Code can connect to it. When ActivePort>0, main falls back to subprocess
// mode so the proxy keeps serving.
//
// All three subpackages write to this variable after a successful embedded
// Start; only one embedded proxy runs per process, so there is no need for
// per-provider slots.
var ActivePort int
