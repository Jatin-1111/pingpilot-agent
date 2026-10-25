// Package config says where the agent sends to and who it is. Settings come from flags, then environment variables, then
// a small file, so an install script, a container and a person at a terminal can each set them the way that suits.
// The token is the one secret: it is read from a file or the environment, never from a flag, because a flag is visible to
// every user on the machine in the process list.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
)

// DefaultAPI is where hosted Ping Pilott lives. A self-hosted one is set with PINGPILOT_API.
const DefaultAPI = "https://ping-pilott-backend.onrender.com"

var tokenShape = regexp.MustCompile(`^ppa_[0-9a-f]{24}_[A-Za-z0-9_-]{43}$`)

type Config struct {
	API       string
	Token     string
	TokenFrom string // where the token was found, for messages ("the environment", a file path)
}

// Inputs are what the command line, the environment and the files offered. Tests pass their own.
type Inputs struct {
	APIFlag       string
	ConfigFlag    string
	TokenFileFlag string
	Env           func(string) string
	ReadFile      func(string) ([]byte, error)
	Stat          func(string) (os.FileInfo, error)
	GOOS          string
}

func (in *Inputs) defaults() {
	if in.Env == nil {
		in.Env = os.Getenv
	}
	if in.ReadFile == nil {
		in.ReadFile = os.ReadFile
	}
	if in.Stat == nil {
		in.Stat = os.Stat
	}
	if in.GOOS == "" {
		in.GOOS = runtime.GOOS
	}
}

// DefaultDir is where an install keeps the agent's files.
func DefaultDir(goos string) string {
	if goos == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return strings.TrimRight(pd, `\/`) + `\pingpilot-agent`
		}
		return `C:\ProgramData\pingpilot-agent`
	}
	return "/etc/pingpilot-agent"
}

// inDir is a file in the agent's directory, written the way the target system writes paths (not the way the machine the
// code happens to run on does, which only matters in tests but is the difference between a test and a guess).
func inDir(goos, name string) string {
	if goos == "windows" {
		return DefaultDir(goos) + `\` + name
	}
	return DefaultDir(goos) + "/" + name
}

type fileConfig struct {
	API       string `json:"api"`
	TokenFile string `json:"tokenFile"`
}

// Load works out the settings. It returns what it can and an error that says what to do when the token is missing.
func Load(in Inputs) (Config, error) {
	in.defaults()

	var file fileConfig
	configPath := first(in.ConfigFlag, in.Env("PINGPILOT_CONFIG"), inDir(in.GOOS, "agent.json"))
	explicit := in.ConfigFlag != "" || in.Env("PINGPILOT_CONFIG") != ""
	if raw, err := in.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(raw, &file); err != nil {
			return Config{}, fmt.Errorf("%s is not valid JSON: %v", configPath, err)
		}
	} else if explicit {
		return Config{}, fmt.Errorf("cannot read the config file %s: %v", configPath, err)
	}

	cfg := Config{API: first(in.APIFlag, in.Env("PINGPILOT_API"), file.API, DefaultAPI)}
	cfg.API = strings.TrimRight(cfg.API, "/")
	if err := ValidateAPI(cfg.API); err != nil {
		return cfg, err
	}

	// The token: the environment first (a container), then a file (an install)
	if t := strings.TrimSpace(in.Env("PINGPILOT_TOKEN")); t != "" {
		cfg.Token, cfg.TokenFrom = t, "the PINGPILOT_TOKEN environment variable"
		return cfg, nil
	}
	tokenPath := first(in.TokenFileFlag, in.Env("PINGPILOT_TOKEN_FILE"), file.TokenFile, inDir(in.GOOS, "token"))
	raw, err := in.ReadFile(tokenPath)
	if err != nil {
		return cfg, fmt.Errorf("no token found. Put it in %s (or set PINGPILOT_TOKEN). Copy it from the server's page in Ping Pilott: %v", tokenPath, err)
	}
	cfg.Token, cfg.TokenFrom = strings.TrimSpace(string(raw)), tokenPath
	return cfg, nil
}

// ValidateAPI accepts https addresses, and plain http only for the machine itself, so the token is never sent in the clear
// over a network (and a typo cannot quietly downgrade it).
func ValidateAPI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("the API address %q is not a web address", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || net.ParseIP(host).IsLoopback() {
			return nil
		}
		return fmt.Errorf("the API address %q uses plain http: the token would travel unencrypted. Use https", raw)
	}
	return fmt.Errorf("the API address %q must start with https://", raw)
}

// CheckToken says whether a token has the shape of one, without saying why not in a way that repeats it.
func CheckToken(token string) error {
	if token == "" {
		return errors.New("the token is empty")
	}
	if !tokenShape.MatchString(token) {
		return errors.New("that does not look like a Ping Pilott agent token (it should start with ppa_ and be 72 characters). Copy it again from the server's page")
	}
	return nil
}

// Redact shows enough of a token to tell two apart and nothing that works.
func Redact(token string) string {
	if len(token) < 8 {
		return "..."
	}
	return token[:8] + "..."
}

// RedactIn removes a token from any text, so an error that happens to contain it is safe to print.
func RedactIn(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, Redact(token))
}

// LooseTokenFile reports a token file that other users on the machine could read. Only meaningful on Unix.
func LooseTokenFile(info os.FileInfo, goos string) bool {
	return goos != "windows" && info.Mode().Perm()&0o077 != 0
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
