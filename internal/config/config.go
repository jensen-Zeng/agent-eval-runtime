package config

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type LLM struct {
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	APIKeyEnv  string `json:"api_key_env"`
	MaxRetries int    `json:"max_retries"`
}

type Server struct {
	Listen          string `json:"listen"`
	Workers         int    `json:"workers"`
	QueueCapacity   int    `json:"queue_capacity"`
	PollIntervalMS  int    `json:"poll_interval_ms"`
	ShutdownSeconds int    `json:"shutdown_seconds"`
	DatabaseURLEnv  string `json:"database_url_env"`
}

func (s Server) Validate() error {
	if s.Workers < 1 || s.Workers > 8 || s.QueueCapacity < 1 || s.QueueCapacity > 1000 || s.PollIntervalMS < 50 || s.PollIntervalMS > 5000 || s.ShutdownSeconds < 1 || s.ShutdownSeconds > 120 {
		return fmt.Errorf("invalid server worker, queue, polling or shutdown limits")
	}
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(s.DatabaseURLEnv) {
		return fmt.Errorf("invalid database URL environment variable")
	}
	host, port, err := net.SplitHostPort(s.Listen)
	if err != nil {
		return fmt.Errorf("server listen must be a loopback host:port")
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("server must listen on loopback")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("invalid server port")
	}
	return nil
}

type Config struct {
	Image        string `json:"image"`
	DockerHost   string `json:"docker_host"`
	ArtifactsDir string `json:"artifacts_dir"`
	LLM          LLM    `json:"llm"`
	Server       Server `json:"server"`
}

func Default() Config {
	return Config{
		Image: "agenteval-sandbox:v1.0", ArtifactsDir: "artifacts",
		LLM:    LLM{BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY", MaxRetries: 2},
		Server: Server{Listen: "127.0.0.1:8787", Workers: 2, QueueCapacity: 32, PollIntervalMS: 250, ShutdownSeconds: 30, DatabaseURLEnv: "AGENTEVAL_DATABASE_URL"},
	}
}

func Load(file string) (Config, error) {
	c := Default()
	if file != "" {
		f, e := os.Open(file)
		if e != nil {
			return c, e
		}
		defer f.Close()
		if e = task.DecodeStrict(f, &c); e != nil {
			return c, e
		}
	}
	if c.Image == "" || c.ArtifactsDir == "" || c.LLM.APIKeyEnv == "" || c.LLM.MaxRetries < 0 || c.LLM.MaxRetries > 2 {
		return c, fmt.Errorf("invalid configuration")
	}
	if err := c.Server.Validate(); err != nil {
		return c, err
	}
	if c.LLM.Model == "" {
		c.LLM.Model = os.Getenv("OPENAI_MODEL")
	}
	return c, nil
}
