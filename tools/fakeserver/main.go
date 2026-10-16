// fakeserver stands in for llama-server (default) or a cloud provider
// (--role cloud) so the whole pipeline - provisioning, gates, escalation,
// redaction, ledger - can be exercised without downloading a model.
//
// Prompts containing "hallucinate" make the local role answer with a tool
// that does not exist; "broken" makes it answer with a syntax error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

func main() {
	fs := flag.NewFlagSet("fakeserver", flag.ExitOnError)
	role := fs.String("role", "llama", "llama | cloud")
	port := fs.Int("port", 8080, "")
	host := fs.String("host", "127.0.0.1", "")
	fs.String("m", "", "model path")
	fs.Int("t", 0, "")
	fs.Int("tb", 0, "")
	fs.Int("c", 0, "")
	fs.Int("np", 0, "")
	fs.Int("ngl", 0, "")
	keyFile := fs.String("api-key-file", "", "require this bearer key (like llama-server)")
	_ = fs.Parse(os.Args[1:])
	apiKey := ""
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			log.Fatalf("api key file: %v", err)
		}
		apiKey = strings.TrimSpace(string(b))
	}

	var ready atomic.Bool
	loadMS := 800
	fmt.Sscan(os.Getenv("FAKE_LOAD_MS"), &loadMS)
	if *role == "cloud" {
		loadMS = 0
	}
	go func() {
		time.Sleep(time.Duration(loadMS) * time.Millisecond) // simulated weight load
		ready.Store(true)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, `{"error":"loading model"}`, http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role, Content string
			} `json:"messages"`
			ResponseFormat any `json:"response_format"`
		}
		body := make([]byte, 0)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		if dump := os.Getenv("FAKE_DUMP"); dump != "" {
			_ = os.WriteFile(dump, body, 0o600)
		}
		_ = json.Unmarshal(body, &req)
		if apiKey != "" && r.Header.Get("Authorization") != "Bearer "+apiKey {
			http.Error(w, `{"error":{"message":"invalid api key"}}`, http.StatusUnauthorized)
			return
		}
		if *role == "cloud" && r.Header.Get("Authorization") == "" {
			http.Error(w, `{"error":{"message":"missing key"}}`, http.StatusUnauthorized)
			return
		}
		var all, firstUser string
		for _, m := range req.Messages {
			all += m.Content + "\n"
			if m.Role == "user" && firstUser == "" {
				firstUser = m.Content
			}
		}
		isCmd := strings.Contains(all, `"command"`)
		text, lp := answer(*role, firstUser, all, isCmd)
		time.Sleep(150 * time.Millisecond) // simulated generation
		lps := make([]map[string]any, 12)
		for i := range lps {
			lps[i] = map[string]any{"token": "x", "logprob": lp}
		}
		resp := map[string]any{
			"model": map[string]string{"llama": "fake-local", "cloud": "fake-cloud"}[*role],
			"choices": []any{map[string]any{
				"message":  map[string]any{"role": "assistant", "content": text},
				"logprobs": map[string]any{"content": lps},
			}},
			"usage": map[string]int{"prompt_tokens": len(all) / 4, "completion_tokens": len(text) / 4},
		}
		json.NewEncoder(w).Encode(resp)
	})
	addr := fmt.Sprintf("%s:%d", *host, *port)
	log.Printf("fakeserver role=%s listening on %s", *role, addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func cmd(c, e string) string {
	b, _ := json.Marshal(map[string]string{"command": c, "explanation": e})
	return string(b)
}

func answer(role, user, all string, isCmd bool) (string, float64) {
	u := strings.ToLower(user)
	if !isCmd {
		if role == "cloud" {
			return "Cloud answer: the log shows the database connection was refused; check that Postgres is running on port 5432.", -0.1
		}
		return "Local answer: " + strings.SplitN(user, "\n", 2)[0], -0.08
	}
	if role == "cloud" {
		if strings.Contains(all, "failed automatic checks") {
			return cmd("find . -type f -name '*.go' -exec grep -l 'TODO' {} +", "Lists Go files that contain TODO."), -0.05
		}
		return cmd("du -ah . | sort -rh | head -n 10", "Shows the 10 largest files and directories."), -0.05
	}
	switch {
	case strings.Contains(u, "hallucinate"):
		return cmd("frobnicate --all-files --todo", "Finds TODOs."), -0.6
	case strings.Contains(u, "broken"):
		return cmd(`find . -name "*.go" | (`, "Finds Go files."), -0.1
	case strings.Contains(u, "large") || strings.Contains(u, "100mb"):
		return cmd("find . -type f -size +100M", "Lists files larger than 100 MB."), -0.04
	case strings.Contains(u, "delete") || strings.Contains(u, "clean"):
		return cmd("rm -rf ./build", "Removes the build directory."), -0.05
	}
	return cmd("ls -la", "Lists files in the current directory."), -0.05
}
