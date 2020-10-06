package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/github/hub/v2/git"
	"github.com/github/hub/v2/github"
	"github.com/gorilla/websocket"
	"github.com/peterbourgon/ff/v3"
	"github.com/peterbourgon/ff/v3/ffcli"
	"go.uber.org/zap"
	"moul.io/motd"
	"moul.io/zapconfig"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

var (
	fs                = flag.NewFlagSet("git-ci", flag.ExitOnError)
	githubUserSession = fs.String("github-user-session", "", `value of the "user_session" cookie on "github.com"`)
	debug             = fs.Bool("debug", false, "log debug information")
	logger            *zap.Logger
)

func run(args []string) error {
	// setup & parse CLI
	root := &ffcli.Command{
		ShortUsage: "git-ci [flags] <subcommand>",
		ShortHelp:  "interract with CI/CD from command line",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarNoPrefix()},
		Exec: func(ctx context.Context, args []string) error {
			fmt.Print(motd.Default())
			return flag.ErrHelp
		},
		Subcommands: []*ffcli.Command{
			// login
			// wait
			// tail
			{Name: "status", Exec: doStatus},
		},
	}
	if err := root.Parse(args[1:]); err != nil {
		return err
	}

	// setup logger
	{
		config := zapconfig.Configurator{}
		if *debug {
			config.SetLevel(zap.DebugLevel)
		} else {
			config.SetLevel(zap.InfoLevel)
		}
		logger = config.MustBuild()
		logger.Debug("init",
			zap.String("github-user-session", sensitiveData(*githubUserSession)),
			zap.Bool("debug", *debug),
		)
	}

	// run CLI command
	return root.Run(context.Background())
}

func doStatus(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return flag.ErrHelp
	}
	if *githubUserSession == "" {
		logger.Error("missing -github-user-session OR $GITHUB_USER_SESSION")
		return flag.ErrHelp
	}

	// parse local repo
	ref := "HEAD"
	localRepo, err := github.LocalRepo()
	if err != nil {
		return fmt.Errorf("get local repo failed: %w", err)
	}
	project, err := localRepo.MainProject()
	if err != nil {
		return fmt.Errorf("get project from local repo failed: %w", err)
	}
	projectPrefix := fmt.Sprintf("%s://%s/%s/%s", project.Protocol, project.Host, project.Owner, project.Name)
	sha, err := git.Ref(ref)
	if err != nil {
		return fmt.Errorf("no revision could be determined from %q: %w", ref, err)
	}

	// fetch CI statuses from GitHub API using 'hub'
	gh := github.NewClient(project.Host)
	response, err := gh.FetchCIStatus(project, sha)
	if err != nil {
		return fmt.Errorf("fetch CI statuses failed, check your 'hub' configuration: %w", err)
	}

	var wg sync.WaitGroup
	for _, status := range response.Statuses {
		switch {
		case strings.HasPrefix(status.TargetURL, projectPrefix): // GitHub actions
			logger.Debug("GitHub action run", zap.String("url", status.TargetURL), zap.String("state", status.State))
			if status.State == "success" {
				continue
			}
			wg.Add(1)
			checkID := strings.Split(status.TargetURL, "/")[6]
			liveLogsURL := fmt.Sprintf("%s/commit/%s/checks/%s/live_logs", projectPrefix, sha, checkID)
			name := checkID // FIXME: get job name
			go func(url, name string) {
				// FIXME: the websocket stream won't stop, we should subscribe to an activity stream to detect that a stream is finished
				err := streamGitHubCheckLiveLogs(ctx, url, name)
				if err != nil {
					logger.Error("stream github check live logs", zap.Error(err))
				}
				wg.Done()
			}(liveLogsURL, name)
		default:
			logger.Debug("unsupported CI target", zap.String("url", status.TargetURL), zap.String("state", status.State))
		}
	}

	wg.Wait()
	return nil
}

// nolint:gocognit,gocyclo // only a PoC for now, this will be completely rewritten
func streamGitHubCheckLiveLogs(ctx context.Context, liveLogsURL, name string) error {
	// step 1: query live_logs URL to get an authenticated URL on the pipeline service
	var pipelineAuthenticatedURL string
	{
		logger.Debug("compute live logs URL", zap.String("url", liveLogsURL))
		req, err := http.NewRequestWithContext(ctx, "GET", liveLogsURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Cookie", fmt.Sprintf("user_session=%s", *githubUserSession))
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var ret struct {
			Success bool          `json:"success"`
			Error   string        `json:"error"`
			Errors  []interface{} `json:"errors"`
			Data    struct {
				AuthenticatedURL string `json:"authenticated_url"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&ret); err != nil {
			return err
		}
		if ret.Error == "Not Found" {
			logger.Debug("not an action with logs, i.e. WIP Status Checker, skipping")
			return nil
		}
		pipelineAuthenticatedURL = ret.Data.AuthenticatedURL
	}

	// step 2: query the authenticated URL to get the WebSocket URL
	var wsURL string
	{
		logger.Debug("get authenticated URL", zap.String("url", pipelineAuthenticatedURL))
		req, err := http.NewRequestWithContext(ctx, "GET", pipelineAuthenticatedURL, nil)
		if err != nil {
			return err
		}
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var ret struct {
			LogStreamWebSocketURL string `json:"logStreamWebSocketUrl"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&ret); err != nil {
			return err
		}
		wsURL = ret.LogStreamWebSocketURL
	}

	// step 3: connect to the websocket server
	{
		jsonTerminator := []byte{0x1E}

		logger.Debug("connecting to websocket", zap.String("url", wsURL))
		c, _, err := websocket.DefaultDialer.Dial(wsURL, nil) // nolint:bodyclose // false positive, body is closed in few lines
		if err != nil {
			return err
		}
		defer c.Close()

		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				_, message, err := c.ReadMessage()
				if err != nil {
					logger.Error("read error", zap.Error(err))
					return
				}
				for _, part := range bytes.Split(message, jsonTerminator) {
					if len(bytes.TrimSpace(part)) == 0 {
						continue
					}
					logger.Debug("recv", zap.String("message", string(part)))
					var parsed struct {
						Type      int    `json:"type"`
						Target    string `json:"target"`
						Arguments []struct {
							RunID            int      `json:"runId"`
							TimelineID       string   `json:"timelineId"`
							TimelineRecordID string   `json:"timelineRecordId"`
							StepRecordID     string   `json:"stepRecordId"`
							StartLine        int      `json:"startLine"`
							Lines            []string `json:"lines"`
						} `json:"arguments"`
					}
					if err := json.Unmarshal(part, &parsed); err != nil {
						logger.Error("message unmarshal", zap.Error(err))
						return
					}

					// nolint:gomnt // not relevant here
					switch {
					case string(part) == "{}":
						// noop
					case parsed.Type == 1 && parsed.Target == "logConsoleLines" && len(parsed.Arguments) == 1:
						for _, line := range parsed.Arguments[0].Lines {
							fmt.Printf("%s | %s\n", name, line)
							if line == "Cleanup up orphan processes" { // FIXME: this check will be removed as soon as we detect the state change from an activity stream
								return
							}
						}
					case parsed.Type == 6: // no idea what it is, but it happens very often
						// noop
					default:
						logger.Warn("unsupported websocket message", zap.String("message", string(part)))
					}
				}
			}
		}()

		// determine tenantID and runID
		var tenantID, runID string
		{
			u, err := url.Parse(wsURL)
			if err != nil {
				return err
			}
			tenantID = u.Query().Get("tenantId")
			runID = u.Query().Get("runId")
		}

		// handshake
		{
			msg := `{"protocol":"json","version":1}`
			logger.Debug("sending handshake message", zap.String("message", msg))
			if err := c.WriteMessage(websocket.TextMessage, append([]byte(msg), jsonTerminator...)); err != nil {
				return err
			}
			msg = fmt.Sprintf(`{"arguments":["%s",%s],"target":"WatchRunAsync","type":1}`, tenantID, runID)
			logger.Debug("sending handshake message", zap.String("message", msg))
			if err := c.WriteMessage(websocket.TextMessage, append([]byte(msg), jsonTerminator...)); err != nil {
				return err
			}
		}

		//
		for {
			select {
			case <-done:
				return nil
			case <-ctx.Done():
				logger.Info("context canceled, sending clean websocket close")
				err := c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				if err != nil {
					return fmt.Errorf("write close: %w", err)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
				}
				return nil
			}
		}
	}
}

func sensitiveData(input string) string {
	if input != "" {
		return "*REDACTED*"
	}
	return ""
}
