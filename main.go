package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/alexflint/go-arg"
	"github.com/google/go-github/v68/github"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/push"
	"github.com/prometheus/common/expfmt"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"tailscale.com/tsnet"
)

// constants settable at build time
var (
	Version = "1.0.0"
)

var (
	registry = prometheus.NewRegistry()

	repoCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "github_repo_count",
			Help: "The total number of repositories",
		},
		[]string{"owner", "visibility", "archived"},
	)

	issueCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "github_issue_count",
			Help: "The count of issues or pulls",
		},
		[]string{"github_repo", "type", "state"},
	)

	notificationCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "github_notification_count",
			Help: "The number of notifications",
		},
		[]string{"unread"},
	)

	workflowRunNumber = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "github_workflow_run_number",
			Help: "The latest run number for a workflow.",
		},
		[]string{"github_repo", "workflow_name"},
	)

	workflowRunState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "github_workflow_run_conclusion",
			Help: "The latest state of a workflow run.",
		},
		[]string{"github_repo", "workflow_name", "github_workflow_run_conclusion"},
	)
)

func init() {
	registry.MustRegister(repoCount)
	registry.MustRegister(issueCount)
	registry.MustRegister(notificationCount)
	registry.MustRegister(workflowRunNumber)
	registry.MustRegister(workflowRunState)
}

func updateGitHubMetrics(client *github.Client, ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		if err := updateNotificationsMetrics(ctx, client); err != nil {
			return fmt.Errorf("notifications metrics: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		if err := updateIssueMetrics(ctx, client); err != nil {
			return fmt.Errorf("issue metrics: %w", err)
		}
		return nil
	})

	g.Go(func() error {
		repos, err := fetchUserRepos(ctx, client)
		if err != nil {
			return fmt.Errorf("fetching repos: %w", err)
		}

		repoGroup, ctx := errgroup.WithContext(ctx)

		repoGroup.Go(func() error {
			if err := updateRepoCountMetrics(ctx, repos); err != nil {
				return fmt.Errorf("repo count metrics: %w", err)
			}
			return nil
		})

		for _, repo := range repos {
			if repo.GetArchived() {
				continue
			}
			repoGroup.Go(func() error {
				if err := updateWorkflowRunMetrics(ctx, client, repo); err != nil {
					return fmt.Errorf("workflow metrics for %s: %w", repo.GetFullName(), err)
				}
				return nil
			})
		}
		return repoGroup.Wait()
	})

	return g.Wait()
}

func updateNotificationsMetrics(ctx context.Context, client *github.Client) error {
	notifications, _, err := client.Activity.ListNotifications(ctx, nil)
	if err != nil {
		return err
	}

	unreadCount := 0
	for _, notification := range notifications {
		if notification.Unread != nil && *notification.Unread {
			unreadCount++
		}
	}
	notificationCount.With(prometheus.Labels{"unread": "true"}).Set(float64(unreadCount))

	return nil
}

func updateRepoCountMetrics(ctx context.Context, repos []*github.Repository) error {
	repoCounts := make(map[string]map[string]map[string]int)

	for _, repo := range repos {
		owner := repo.GetOwner().GetLogin()

		visibility := "public"
		if repo.GetPrivate() {
			visibility = "private"
		}

		archived := "false"
		if repo.GetArchived() {
			archived = "true"
		}

		if repoCounts[owner] == nil {
			repoCounts[owner] = make(map[string]map[string]int)
		}
		if repoCounts[owner][visibility] == nil {
			repoCounts[owner][visibility] = make(map[string]int)
		}

		repoCounts[owner][visibility][archived]++
	}

	for owner, visCounts := range repoCounts {
		for visibility, archCounts := range visCounts {
			for archived, count := range archCounts {
				repoCount.With(prometheus.Labels{
					"owner":      owner,
					"visibility": visibility,
					"archived":   archived,
				}).Set(float64(count))
			}
		}
	}

	return nil
}

func fetchUserRepos(ctx context.Context, client *github.Client) ([]*github.Repository, error) {
	opts := &github.RepositoryListByAuthenticatedUserOptions{
		Type:      "owner",
		Sort:      "full_name",
		Direction: "asc",
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}

	var allRepos []*github.Repository
	for {
		repos, resp, err := client.Repositories.ListByAuthenticatedUser(ctx, opts)
		if err != nil {
			return nil, err
		}

		for _, repo := range repos {
			if repo != nil {
				allRepos = append(allRepos, repo)
			}
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allRepos, nil
}

func updateWorkflowRunMetrics(ctx context.Context, client *github.Client, repo *github.Repository) error {
	owner, repoName := repo.GetOwner().GetLogin(), repo.GetName()

	runs, _, err := client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repoName, &github.ListWorkflowRunsOptions{
		Branch: repo.GetDefaultBranch(),
		Status: "completed",
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	})
	if err != nil {
		return err
	}

	latestRuns := make(map[int64]*github.WorkflowRun)
	for _, run := range runs.WorkflowRuns {
		workflowID := run.GetWorkflowID()
		if existing, ok := latestRuns[workflowID]; !ok || run.GetRunNumber() > existing.GetRunNumber() {
			latestRuns[workflowID] = run
		}
	}

	workflows, _, err := client.Actions.ListWorkflows(ctx, owner, repoName, &github.ListOptions{})
	if err != nil {
		return err
	}

	for _, workflow := range workflows.Workflows {
		if latestRun, ok := latestRuns[workflow.GetID()]; ok {
			workflowRunNumber.With(prometheus.Labels{
				"github_repo":   *repo.FullName,
				"workflow_name": workflow.GetName(),
			}).Set(float64(latestRun.GetRunNumber()))

			conclusions := []string{"action_required", "cancelled", "failure", "neutral",
				"skipped", "stale", "startup_failure", "success", "timed_out"}
			for _, conclusion := range conclusions {
				value := 0.0
				if conclusion == latestRun.GetConclusion() {
					value = 1.0
				}
				workflowRunState.With(prometheus.Labels{
					"github_repo":                    *repo.FullName,
					"workflow_name":                  workflow.GetName(),
					"github_workflow_run_conclusion": conclusion,
				}).Set(value)
			}
		}
	}

	return nil
}

const issuesGraphQLQuery = `
query($login: String!) {
	user(login: $login) {
		repositories(first: 100, affiliations: OWNER, isArchived: false) {
			nodes {
				nameWithOwner
				openIssues: issues(states: OPEN) { totalCount }
				closedIssues: issues(states: CLOSED) { totalCount }
				openPulls: pullRequests(states: OPEN) { totalCount }
				closedPulls: pullRequests(states: CLOSED) { totalCount }
			}
		}
	}
}`

type graphQLIssuesResponse struct {
	Data struct {
		User struct {
			Repositories struct {
				Nodes []struct {
					NameWithOwner string `json:"nameWithOwner"`
					OpenIssues    struct {
						TotalCount int `json:"totalCount"`
					} `json:"openIssues"`
					ClosedIssues struct {
						TotalCount int `json:"totalCount"`
					} `json:"closedIssues"`
					OpenPulls struct {
						TotalCount int `json:"totalCount"`
					} `json:"openPulls"`
					ClosedPulls struct {
						TotalCount int `json:"totalCount"`
					} `json:"closedPulls"`
				} `json:"nodes"`
			} `json:"repositories"`
		} `json:"user"`
	} `json:"data"`
}

func updateIssueMetrics(ctx context.Context, client *github.Client) error {
	user, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return err
	}
	username := user.GetLogin()

	variables := map[string]any{
		"login": username,
	}

	var response graphQLIssuesResponse
	if err := executeGraphQL(client, ctx, issuesGraphQLQuery, variables, &response); err != nil {
		return err
	}

	for _, repo := range response.Data.User.Repositories.Nodes {
		issueCount.With(prometheus.Labels{
			"github_repo": repo.NameWithOwner,
			"type":        "issue",
			"state":       "open",
		}).Set(float64(repo.OpenIssues.TotalCount))

		issueCount.With(prometheus.Labels{
			"github_repo": repo.NameWithOwner,
			"type":        "issue",
			"state":       "closed",
		}).Set(float64(repo.ClosedIssues.TotalCount))

		issueCount.With(prometheus.Labels{
			"github_repo": repo.NameWithOwner,
			"type":        "pull",
			"state":       "open",
		}).Set(float64(repo.OpenPulls.TotalCount))

		issueCount.With(prometheus.Labels{
			"github_repo": repo.NameWithOwner,
			"type":        "pull",
			"state":       "closed",
		}).Set(float64(repo.ClosedPulls.TotalCount))
	}

	return nil
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func executeGraphQL(client *github.Client, ctx context.Context, query string, variables map[string]any, response any) error {
	req := graphQLRequest{
		Query:     query,
		Variables: variables,
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(req); err != nil {
		return err
	}

	graphqlReq, err := http.NewRequestWithContext(ctx, "POST", "https://api.github.com/graphql", &buf)
	if err != nil {
		return err
	}

	resp, err := client.Client().Do(graphqlReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return json.NewDecoder(resp.Body).Decode(response)
}

func writeToStdout(reg *prometheus.Registry) error {
	enc := expfmt.NewEncoder(os.Stdout, expfmt.NewFormat(expfmt.TypeTextPlain))
	mfs, err := reg.Gather()
	if err != nil {
		return err
	}
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return err
		}
	}
	return nil
}

type loggingRoundTripper struct {
	wrapped http.RoundTripper
}

func (l loggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	fmt.Fprintf(os.Stderr, "[%s] %s %s\n", time.Now().Format(time.RFC3339), req.Method, req.URL)
	return l.wrapped.RoundTrip(req)
}

type generateCommand struct {
	Output             string  `arg:"-o,--output,env:GITHUB_EXPORTER_OUTPUT" placeholder:"FILE"`
	PushgatewayURL     url.URL `arg:"-p,--pushgateway-url,env:GITHUB_EXPORTER_PUSHGATEWAY_URL" placeholder:"URL"`
	PushgatewayRetries int     `arg:"-r,--pushgateway-retries,env:GITHUB_EXPORTER_PUSHGATEWAY_RETRIES" default:"1" placeholder:"RETRIES"`
}

type serveCommand struct {
	Addr     string        `arg:"-l,--listen,env:GITHUB_EXPORTER_LISTEN" default:":9448" placeholder:"ADDRESS:PORT"`
	Interval time.Duration `arg:"-i,--interval,env:GITHUB_EXPORTER_INTERVAL" default:"15m" placeholder:"INTERVAL"`
}

type mainCommand struct {
	Token             string           `arg:"-t,--token,env:GITHUB_TOKEN" placeholder:"TOKEN"`
	TailscaleAuthKey  string           `arg:"--ts-authkey,env:TS_AUTHKEY" placeholder:"KEY"`
	TailscaleHostname string           `arg:"--ts-hostname,env:TS_HOSTNAME" default:"github_exporter" placeholder:"HOSTNAME"`
	Verbose           bool             `arg:"-v,--verbose,env:GITHUB_EXPORTER_VERBOSE" help:"Enable verbose logging"`
	Version           bool             `arg:"-V,--version" help:"Print version information"`
	Generate          *generateCommand `arg:"subcommand:generate"`
	Serve             *serveCommand    `arg:"subcommand:serve"`
}

func fetchGitHubToken() string {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return token
	}
	if token := os.Getenv("GH_TOKEN"); token != "" {
		return token
	}

	if credsDir := os.Getenv("CREDENTIALS_DIRECTORY"); credsDir != "" {
		filenames := []string{"GITHUB_TOKEN", "GH_TOKEN", "github-token", "gh-token"}
		for _, filename := range filenames {
			filepath := credsDir + "/" + filename
			if data, err := os.ReadFile(filepath); err == nil {
				token := string(bytes.TrimSpace(data))
				if token != "" {
					return token
				}
			}
		}
	}

	return ""
}

func main() {
	var args mainCommand
	p := arg.MustParse(&args)

	if args.Version {
		fmt.Println(Version)
		os.Exit(0)
	}

	if args.Token == "" {
		args.Token = fetchGitHubToken()
	}

	if args.Token == "" {
		p.WriteUsage(os.Stderr)
		fmt.Fprintln(os.Stderr, "error: --token is required (or environment variable GITHUB_TOKEN)")
		os.Exit(1)
	}

	ctx := context.Background()

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: args.Token},
	)
	httpClient := oauth2.NewClient(ctx, ts)
	if args.Verbose {
		httpClient.Transport = &loggingRoundTripper{wrapped: httpClient.Transport}
	}
	client := github.NewClient(httpClient)

	var tsServer *tsnet.Server
	if args.TailscaleAuthKey != "" && args.TailscaleHostname != "" {
		tsServer = new(tsnet.Server)
		tsServer.Hostname = args.TailscaleHostname
		tsServer.Ephemeral = args.Generate != nil
		tsServer.AuthKey = args.TailscaleAuthKey
		if args.Verbose {
			tsServer.Logf = log.New(os.Stderr, fmt.Sprintf("[tsnet:%s] ", tsServer.Hostname), log.LstdFlags).Printf
			tsServer.UserLogf = log.New(os.Stderr, fmt.Sprintf("[tsnet:%s] ", tsServer.Hostname), log.LstdFlags).Printf
		}
	}

	switch {
	case args.Generate != nil:
		if err := updateGitHubMetrics(client, ctx); err != nil {
			log.Fatalf("Error fetching metrics: %v", err)
		}

		// If no output or pushgateway is specified, write to stdout
		if args.Generate.Output == "" && args.Generate.PushgatewayURL.String() == "" {
			args.Generate.Output = "-"
		}

		if args.Generate.Output == "-" {
			if err := writeToStdout(registry); err != nil {
				log.Fatalf("Error writing metrics: %v", err)
			}
		} else if args.Generate.Output != "" {
			if err := prometheus.WriteToTextfile(args.Generate.Output, registry); err != nil {
				log.Fatalf("Error writing metrics: %v", err)
			}
		}

		if args.Generate.PushgatewayURL.String() != "" {
			pushHTTPClient := http.DefaultClient

			if tsServer != nil {
				if err := tsServer.Start(); err != nil {
					log.Fatalf("Error starting Tailscale server: %v", err)
				}
				defer tsServer.Close()
				pushHTTPClient = tsServer.HTTPClient()
			}

			pusher := push.New(args.Generate.PushgatewayURL.String(), "github").Client(pushHTTPClient).Gatherer(registry)
			var err error
			for i := 1; i < args.Generate.PushgatewayRetries; i++ {
				if err = pusher.Push(); err == nil {
					break
				}
				log.Printf("Error pushing metrics, retrying (%d/%d): %v", i, args.Generate.PushgatewayRetries, err)
				time.Sleep(2 * time.Second)
			}
			if err != nil {
				log.Fatalf("Error pushing metrics after %d retries: %v", args.Generate.PushgatewayRetries, err)
			}
		}

	case args.Serve != nil:
		go func() {
			log.Printf("[%s] Updating GitHub metrics", time.Now().Format(time.RFC3339))
			if err := updateGitHubMetrics(client, ctx); err != nil {
				log.Printf("[%s] Error fetching metrics: %v", time.Now().Format(time.RFC3339), err)
			}

			for range time.Tick(args.Serve.Interval) {
				log.Printf("[%s] Updating GitHub metrics", time.Now().Format(time.RFC3339))
				if err := updateGitHubMetrics(client, ctx); err != nil {
					log.Printf("[%s] Error fetching GitHub metrics: %v", time.Now().Format(time.RFC3339), err)
				}
			}
		}()

		if tsServer != nil {
			if err := tsServer.Start(); err != nil {
				log.Fatalf("Error starting Tailscale server: %v", err)
			}
			defer tsServer.Close()
		}

		var ln net.Listener
		var err error
		if tsServer != nil {
			ln, err = tsServer.Listen("tcp", args.Serve.Addr)
		} else {
			ln, err = net.Listen("tcp", args.Serve.Addr)
		}
		if err != nil {
			log.Fatalf("Error listening on %s: %v", args.Serve.Addr, err)
		}
		defer ln.Close()

		http.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{Registry: registry}))
		log.Fatal(http.Serve(ln, nil))

	default:
		p.WriteHelp(os.Stdout)
		os.Exit(1)
	}
}
