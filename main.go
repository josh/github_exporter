package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/google/go-github/v68/github"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
)

var (
	registry = prometheus.NewRegistry()

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
		for _, repo := range repos {
			repo := repo
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
			if repo != nil && !repo.GetArchived() {
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

	variables := map[string]interface{}{
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
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

func executeGraphQL(client *github.Client, ctx context.Context, query string, variables map[string]interface{}, response interface{}) error {
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

func writeMetricsToFile(filename string) error {
	gathering, err := registry.Gather()
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	for _, mf := range gathering {
		_, err := expfmt.MetricFamilyToText(&buf, mf)
		if err != nil {
			return err
		}
	}

	err = os.WriteFile(filename, buf.Bytes(), 0644)
	if err != nil {
		return err
	}

	return nil
}

type loggingRoundTripper struct {
	wrapped http.RoundTripper
}

func (l loggingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	fmt.Printf("%s %s\n", req.Method, req.URL)
	return l.wrapped.RoundTrip(req)
}

func main() {
	ctx := context.Background()

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		log.Fatal("GITHUB_TOKEN environment variable is not set")
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	httpClient := oauth2.NewClient(ctx, ts)
	httpClient.Transport = &loggingRoundTripper{wrapped: httpClient.Transport}
	client := github.NewClient(httpClient)

	if err := updateGitHubMetrics(client, ctx); err != nil {
		log.Fatalf("Error fetching GitHub metrics: %v", err)
	}

	if err := writeMetricsToFile("metrics.prom"); err != nil {
		log.Fatalf("Error writing metrics to file: %v", err)
	}
}
