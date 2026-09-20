package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lgoyal6/codex-relay/internal/httpapi"
	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/service"
)

const defaultDashboardURL = "http://127.0.0.1:7788"

var bootTokenPattern = regexp.MustCompile(`id="codexrelay-boot"[^>]*>\s*(\{[^<]+\})\s*</script>`)

type dashboardClient struct {
	base  string
	token string
	http  *http.Client
}

func newDashboardClient(raw string) (*dashboardClient, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		raw = defaultDashboardURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("dashboard address %q is not a usable http URL", raw)
	}
	c := &dashboardClient{base: raw, http: &http.Client{Timeout: 10 * time.Second}}
	resp, err := c.http.Get(c.base + "/")
	if err != nil {
		return nil, fmt.Errorf("cannot reach codex-relay at %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("could not read the codex-relay dashboard: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex-relay dashboard returned HTTP %d", resp.StatusCode)
	}
	match := bootTokenPattern.FindSubmatch(body)
	if len(match) != 2 {
		return nil, fmt.Errorf("the dashboard did not provide a local API session token")
	}
	var boot struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(match[1], &boot); err != nil || boot.Token == "" {
		return nil, fmt.Errorf("the dashboard API session token was unreadable")
	}
	c.token = boot.Token
	return c, nil
}

func (c *dashboardClient) call(method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Codex-Pool-Token", c.token)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("codex-relay API request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var problem struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &problem)
		if problem.Error == "" {
			problem.Error = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("%s", problem.Error)
	}
	if output != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, output); err != nil {
			return fmt.Errorf("could not read codex-relay's response: %w", err)
		}
	}
	return nil
}

func cmdProfile(args []string) error {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	defaultAddr := strings.TrimSpace(os.Getenv("CODEXRELAY_ADDR"))
	if defaultAddr == "" {
		defaultAddr = defaultDashboardURL
	}
	addr := fs.String("addr", defaultAddr, "running codex-relay dashboard URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: codexrelay profile [--addr URL] [status|profiles|who [N]|COMMAND]")
	}
	remaining := fs.Args()
	command := "status"
	if len(remaining) > 0 {
		command = policy.NormalizeProfileCommand(remaining[0])
	}
	if command == "help" || command == "-h" || command == "--help" {
		printProfileUsage()
		return nil
	}

	client, err := newDashboardClient(*addr)
	if err != nil {
		return err
	}
	switch command {
	case "status":
		var state httpapi.StateResponse
		if err := client.call(http.MethodGet, "/api/state", nil, &state); err != nil {
			return err
		}
		printProfileStatus(state)
		return nil
	case "profiles":
		var state httpapi.StateResponse
		if err := client.call(http.MethodGet, "/api/state", nil, &state); err != nil {
			return err
		}
		printProfiles(state)
		return nil
	case "who":
		limit := 10
		if len(remaining) > 1 {
			parsed, err := strconv.Atoi(remaining[1])
			if err != nil || parsed < 1 || parsed > 2000 {
				return fmt.Errorf("who count must be between 1 and 2000")
			}
			limit = parsed
		}
		var result struct {
			Activity []service.ActivityRow `json:"activity"`
		}
		if err := client.call(http.MethodGet, "/api/activity?limit="+strconv.Itoa(limit), nil, &result); err != nil {
			return err
		}
		printProfileWho(result.Activity)
		return nil
	default:
		var result struct {
			Profile  policy.RoutingProfile `json:"profile"`
			Decision policy.Decision       `json:"decision"`
		}
		path := "/api/profiles/" + url.PathEscape(command) + "/activate"
		if err := client.call(http.MethodPost, path, nil, &result); err != nil {
			return err
		}
		fmt.Printf("active: %s  (relaypool %s)\n", result.Profile.Name, result.Profile.Command)
		fmt.Println(result.Decision.Summary)
		fmt.Println("Eligible existing conversations switch between turns on their next request.")
		return nil
	}
}

func printProfileUsage() {
	fmt.Print(`relaypool uses the profiles configured in the codex-relay dashboard.

Usage:
  relaypool status       Show the active profile and current routing decision
  relaypool profiles     List every command and alias
  relaypool who [N]      Show which workspace served the latest turns
  relaypool COMMAND      Activate that custom profile

The equivalent long form is: codexrelay profile [--addr URL] ...
Set CODEXRELAY_ADDR when the service is not at http://127.0.0.1:7788.
`)
}

func printProfiles(state httpapi.StateResponse) {
	if len(state.Profiles) == 0 {
		fmt.Println("No profiles yet. Create one in the dashboard under Profiles.")
		return
	}
	for _, profile := range state.Profiles {
		marker := " "
		if profile.Active {
			marker = "*"
		}
		aliases := ""
		if len(profile.Aliases) > 0 {
			aliases = "  aliases: " + strings.Join(profile.Aliases, ", ")
		}
		fmt.Printf("%s %-16s %-24s %s%s\n", marker, profile.Command, profile.Name, profile.Sentence, aliases)
	}
}

func printProfileStatus(state httpapi.StateResponse) {
	active := "none"
	for _, profile := range state.Profiles {
		if profile.Active {
			active = fmt.Sprintf("%s  (relaypool %s)", profile.Name, profile.Command)
			break
		}
	}
	fmt.Println("active:", active)
	fmt.Println(state.Proposed.Summary)
	fmt.Println()
	fmt.Printf("%-28s %-11s %-12s %-12s %s\n", "WORKSPACE", "STATUS", "5H LEFT", "WEEK LEFT", "RESETS")
	for _, workspace := range state.Workspaces {
		status := "ready"
		for _, candidate := range state.Proposed.Candidates {
			if candidate.WorkspaceID == workspace.ID && !candidate.Eligible {
				status = string(candidate.Reason)
			}
		}
		five, weekly := findWindow(workspace, 300), findWindow(workspace, policy.WeeklyWindowMinutes)
		resets := "-"
		if weekly != nil && weekly.ResetsAt != nil {
			resets = humanUntil(weekly.ResetsAt.Sub(time.Now()))
		}
		fmt.Printf("%-28s %-11s %-12s %-12s %s\n", truncate(workspace.Name, 28), truncate(status, 11), windowLeft(five), windowLeft(weekly), resets)
	}
	if len(state.PaceStandings) > 0 {
		fmt.Println("\nWeekly pace:")
		name := map[string]string{}
		for _, workspace := range state.Workspaces {
			name[workspace.ID] = workspace.Name
		}
		for _, standing := range state.PaceStandings {
			if !standing.Known {
				fmt.Printf("  %s: %s\n", name[standing.WorkspaceID], standing.Status)
				continue
			}
			fmt.Printf("  %s: %.1f%% left, %.1f%% target now, %+.1f points, %s\n",
				name[standing.WorkspaceID], standing.RemainingPercent, standing.ExpectedPercent,
				standing.DeltaPercent, standing.Status)
		}
	}
}

func findWindow(workspace httpapi.WorkspaceView, minutes int64) *httpapi.WindowView {
	for i := range workspace.Windows {
		if workspace.Windows[i].Minutes == minutes {
			return &workspace.Windows[i]
		}
	}
	return nil
}

func windowLeft(window *httpapi.WindowView) string {
	if window == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", window.RemainingPercent)
}

func printProfileWho(rows []service.ActivityRow) {
	if len(rows) == 0 {
		fmt.Println("No turns routed yet.")
		return
	}
	fmt.Printf("%-17s %-24s %-12s %-18s %9s %9s\n", "WHEN", "WORKSPACE", "OUTCOME", "REASON", "INPUT", "OUTPUT")
	for _, row := range rows {
		input, output := "-", "-"
		if row.InputTokens != nil {
			input = strconv.FormatInt(*row.InputTokens, 10)
		}
		if row.OutputTokens != nil {
			output = strconv.FormatInt(*row.OutputTokens, 10)
		}
		fmt.Printf("%-17s %-24s %-12s %-18s %9s %9s\n",
			row.At.Local().Format("01-02 15:04:05"), truncate(row.WorkspaceName, 24),
			truncate(row.Outcome, 12), truncate(row.Reason, 18), input, output)
	}
}
