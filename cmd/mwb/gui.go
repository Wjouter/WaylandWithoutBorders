package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/lucky-verma/mwb-linux/internal/config"
)

//go:embed web/index.html
var webFS embed.FS

// runGUI serves the local configuration UI on 127.0.0.1 and opens a browser.
// ponytail: localhost-only is the trust boundary — single-user local tool, the
// handlers shell `systemctl --user` as the invoking user, so no auth is added.
func runGUI(args []string) {
	fs := flag.NewFlagSet("gui", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:15199", "address to serve the GUI on")
	cfgPath := fs.String("config", defaultConfigPath(), "path to config.toml")
	open := fs.Bool("open", true, "open the GUI in a browser")
	_ = fs.Parse(args)

	mux := http.NewServeMux()

	index, _ := webFS.ReadFile("web/index.html")
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Read without validation so a missing/incomplete config still
			// renders the form (config.Load would reject a blank host/key).
			var cfg config.Config
			if _, err := toml.DecodeFile(*cfgPath, &cfg); err != nil && !os.IsNotExist(err) {
				writeJSON(w, http.StatusOK, &cfg) // best effort — show what we can
				return
			}
			writeJSON(w, http.StatusOK, &cfg)
		case http.MethodPost:
			var cfg config.Config
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
				return
			}
			if cfg.Host == "" || cfg.Key == "" {
				http.Error(w, "host and key are required", http.StatusBadRequest)
				return
			}
			if err := config.Save(*cfgPath, &cfg); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"saved": *cfgPath})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		state, _ := exec.Command("systemctl", "--user", "is-active", "mwb").Output()
		log, _ := exec.Command("journalctl", "--user", "-u", "mwb", "-n", "20", "--no-pager", "-o", "cat").Output()
		st := string(trimNL(state))
		writeJSON(w, http.StatusOK, map[string]any{
			"active": st == "active",
			"state":  st,
			"log":    string(log),
		})
	})

	mux.HandleFunc("/api/service", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		switch req.Action {
		case "start", "stop", "restart", "enable", "disable":
		default:
			http.Error(w, "invalid action", http.StatusBadRequest)
			return
		}
		out, err := exec.Command("systemctl", "--user", req.Action, "mwb").CombinedOutput()
		if err != nil {
			http.Error(w, string(out)+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ok": req.Action})
	})

	url := "http://" + *addr
	fmt.Printf("mwb GUI serving at %s (config: %s)\n", url, *cfgPath)
	if *open {
		go func() { _ = exec.Command("xdg-open", url).Start() }()
	}
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "gui server error: %v\n", err)
		os.Exit(1)
	}
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "mwb", "config.toml")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
