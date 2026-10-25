package config

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

const goodToken = "ppa_0123456789abcdef01234567_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop-"

func TestTheTokenShapeIsTheOneTheServerMakes(t *testing.T) {
	if err := CheckToken(goodToken[:len(goodToken)-1] + "_"); err != nil {
		t.Errorf("a well-formed token: %v", err)
	}
	for _, bad := range []string{
		"", "ppa_", "ppa_0123456789abcdef01234567_short", goodToken + "x", " " + goodToken, "ppb_0123456789abcdef01234567_" + strings.Repeat("A", 43),
		"ppa_0123456789ABCDEF01234567_" + strings.Repeat("A", 43), "ppa_0123456789abcdef0123456_" + strings.Repeat("A", 43), "ppa_0123456789abcdef01234567_" + strings.Repeat("!", 43),
	} {
		if err := CheckToken(bad); err == nil {
			t.Errorf("%q should not pass", bad)
		}
	}
}

func TestATokenIsNeverShownInFull(t *testing.T) {
	tok := goodToken[:len(goodToken)-1] + "_"
	if r := Redact(tok); r != "ppa_0123..." || strings.Contains(r, "abcdef") {
		t.Errorf("%q", r)
	}
	text := RedactIn("Post failed for Bearer "+tok+" and again "+tok, tok)
	if strings.Contains(text, "ABCDEFGH") {
		t.Errorf("%q", text)
	}
	if RedactIn("nothing here", "") != "nothing here" || Redact("abc") != "..." {
		t.Error("edge cases")
	}
}

func TestOnlySecureAddressesAreAccepted(t *testing.T) {
	for _, ok := range []string{"https://ping-pilott-backend.onrender.com", "https://example.com:8443", "http://localhost:5000", "http://127.0.0.1:5000", "http://[::1]:5000"} {
		if err := ValidateAPI(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://example.com", "http://192.168.1.5:5000", "ftp://example.com", "example.com", "", "https://", "://x", "javascript:alert(1)"} {
		if err := ValidateAPI(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

// files builds a fake disk
func files(m map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if v, ok := m[name]; ok {
			return []byte(v), nil
		}
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestSettingsComeFromFlagsThenTheEnvironmentThenTheFileThenTheDefault(t *testing.T) {
	disk := files(map[string]string{
		"/etc/pingpilot-agent/agent.json": `{"api":"https://from-file.example"}`,
		"/etc/pingpilot-agent/token":      goodToken + "\n",
	})
	load := func(in Inputs) Config {
		in.ReadFile, in.GOOS = disk, "linux"
		if in.Env == nil {
			in.Env = env(nil)
		}
		c, err := Load(in)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	if got := load(Inputs{}).API; got != "https://from-file.example" {
		t.Errorf("file: %s", got)
	}
	if got := load(Inputs{Env: env(map[string]string{"PINGPILOT_API": "https://from-env.example/"})}).API; got != "https://from-env.example" {
		t.Errorf("env, and a trailing slash is trimmed: %s", got)
	}
	if got := load(Inputs{APIFlag: "https://from-flag.example", Env: env(map[string]string{"PINGPILOT_API": "https://from-env.example"})}).API; got != "https://from-flag.example" {
		t.Errorf("flag: %s", got)
	}
	noFile := Inputs{ReadFile: files(map[string]string{"/etc/pingpilot-agent/token": goodToken}), GOOS: "linux", Env: env(nil)}
	if c, _ := Load(noFile); c.API != DefaultAPI {
		t.Errorf("default: %s", c.API)
	}
}

func TestTheTokenIsReadFromTheEnvironmentOrAFileAndNeverAFlag(t *testing.T) {
	disk := files(map[string]string{"/etc/pingpilot-agent/token": "  " + goodToken + "  \n", "/custom/token": "ppa_custom"})
	c, err := Load(Inputs{ReadFile: disk, GOOS: "linux", Env: env(nil)})
	if err != nil || c.Token != goodToken || c.TokenFrom != "/etc/pingpilot-agent/token" {
		t.Errorf("default file, whitespace trimmed: %+v %v", c, err)
	}
	c, _ = Load(Inputs{ReadFile: disk, GOOS: "linux", Env: env(map[string]string{"PINGPILOT_TOKEN": "ppa_from_env"})})
	if c.Token != "ppa_from_env" || !strings.Contains(c.TokenFrom, "PINGPILOT_TOKEN") {
		t.Errorf("env beats the file: %+v", c)
	}
	c, _ = Load(Inputs{ReadFile: disk, GOOS: "linux", Env: env(nil), TokenFileFlag: "/custom/token"})
	if c.Token != "ppa_custom" {
		t.Errorf("token file flag: %+v", c)
	}
	c, _ = Load(Inputs{ReadFile: disk, GOOS: "linux", Env: env(map[string]string{"PINGPILOT_TOKEN_FILE": "/custom/token"})})
	if c.Token != "ppa_custom" {
		t.Errorf("token file env: %+v", c)
	}
}

func TestAMissingTokenSaysWhereToPutIt(t *testing.T) {
	c, err := Load(Inputs{ReadFile: files(nil), GOOS: "linux", Env: env(nil)})
	if err == nil || !strings.Contains(err.Error(), "/etc/pingpilot-agent/token") || !strings.Contains(err.Error(), "PINGPILOT_TOKEN") {
		t.Errorf("%v", err)
	}
	if c.API != DefaultAPI {
		t.Error("the address is still worked out, so the check can try the network")
	}
}

func TestABrokenOrMissingConfigFileIsTreatedAsTheUserWouldExpect(t *testing.T) {
	_, err := Load(Inputs{ReadFile: files(map[string]string{"/etc/pingpilot-agent/agent.json": "{ nope"}), GOOS: "linux", Env: env(nil)})
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("a broken file is an error: %v", err)
	}
	_, err = Load(Inputs{ConfigFlag: "/nope.json", ReadFile: files(nil), GOOS: "linux", Env: env(nil)})
	if err == nil || !strings.Contains(err.Error(), "cannot read the config file") {
		t.Errorf("a file that was asked for and is missing is an error: %v", err)
	}
	_, err = Load(Inputs{ReadFile: files(map[string]string{"/etc/pingpilot-agent/token": goodToken}), GOOS: "linux", Env: env(map[string]string{"PINGPILOT_API": "http://example.com"})})
	if err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Errorf("an insecure address is refused: %v", err)
	}
}

func TestWindowsKeepsItsFilesWhereWindowsKeepsThem(t *testing.T) {
	t.Setenv("ProgramData", `D:\Data`)
	if got := DefaultDir("windows"); got != `D:\Data\pingpilot-agent` {
		t.Errorf("%s", got)
	}
	if DefaultDir("linux") != "/etc/pingpilot-agent" || DefaultDir("darwin") != "/etc/pingpilot-agent" {
		t.Error("unix")
	}
}

type mode os.FileMode

func (m mode) Name() string       { return "token" }
func (m mode) Size() int64        { return 1 }
func (m mode) Mode() fs.FileMode  { return fs.FileMode(m) }
func (m mode) ModTime() time.Time { return time.Time{} }
func (m mode) IsDir() bool        { return false }
func (m mode) Sys() any           { return nil }

func TestATokenFileOthersCanReadIsFlaggedOnUnixOnly(t *testing.T) {
	cases := []struct {
		perm fs.FileMode
		goos string
		want bool
	}{{0o600, "linux", false}, {0o400, "linux", false}, {0o640, "linux", true}, {0o644, "linux", true}, {0o604, "darwin", true}, {0o666, "windows", false}}
	for _, c := range cases {
		if got := LooseTokenFile(mode(c.perm), c.goos); got != c.want {
			t.Errorf("%o on %s: got %v", c.perm, c.goos, got)
		}
	}
	_ = errors.New
}
