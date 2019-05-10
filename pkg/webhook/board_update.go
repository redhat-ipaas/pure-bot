package webhook

import (
	"context"
	"encoding/json"
	"github.com/go-resty/resty"
	"github.com/google/go-github/github"
	"github.com/pkg/errors"
	"github.com/syndesisio/pure-bot/pkg/config"
	"go.uber.org/zap"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type boardUpdate struct{}

func (h *boardUpdate) EventTypesHandled() []string {
	return []string{"issues", "pull_request"}
}

type column struct {
	name                string
	id                  string
	isPostMergePipeline bool
	isInbox             bool
}

var stateMapping = map[string]column{}

var postProcessing = make(map[string]column)

var zenHubApi = "https://api.zenhub.io"

var regex = regexp.MustCompile("(?mi)(?:clos(?:e[sd]?|ing)|fix(?:e[sd]|ing))[^\\s]*\\s+(?:#|https://github.com/.+/issues/)(?P<issue>[0-9]+)")

var doneColumn = &column{}

var inboxColumn = &column{}

func (h *boardUpdate) HandleEvent(eventObject interface{}, gh *github.Client, config config.RepoConfig, logger *zap.Logger) error {

	if "<repo>" == config.Board.GithubRepo {
		logger.Warn("Repo not configured, ignore event")
		return nil
	}

	// initialise from config if needed
	if len(stateMapping) == 0 {

		logger.Info("Initialising state mappings ...")

		for _, col := range config.Board.Columns {
			c := column{col.Name, col.Id, col.PostMergePipeline, col.IsInbox}

			if c.isPostMergePipeline { // the last one flagged as post process will act as doneColumn
				doneColumn = &c
			}

			if c.isInbox { // the last one flagged as post process will act as inbox
				inboxColumn = &c
			}

			for _, event := range col.Events {
				logger.Info("Mapping " + event + " to " + col.Name)
				stateMapping[event] = c
			}
		}

		if doneColumn == nil || doneColumn.id == "" {
			logger.Warn("Missing column definition for `Done`")
		}

		if inboxColumn == nil || inboxColumn.id == "" {
			logger.Warn("Missing column definition for `Inbox`")
		}
	}

	switch event := eventObject.(type) {
	case *github.IssuesEvent:
		return h.handleIssuesEvent(event, gh, config, logger)
	case *github.PullRequestEvent:
		return h.handlePullRequestEvent(event, gh, config, logger)
	default:
		return nil
	}
}

func (h *boardUpdate) handleIssuesEvent(event *github.IssuesEvent, gh *github.Client, config config.RepoConfig, logger *zap.Logger) error {

	var messageType = "issues"

	number := strconv.Itoa(*event.Issue.Number)
	eventKey := messageType + "_" + *event.Action

	logger.Info("<< Event " + eventKey + " on issue " + number + " >>")

	// post processing (from previous event cycle)
	// takes precedence, the event will no be processed further
	if "issues_closed" == eventKey {

		// ignore labels present?
		labels, _, err := gh.Issues.ListLabelsByIssue(
			context.Background(),
			event.Repo.Owner.GetLogin(),
			event.Repo.GetName(),
			*event.Issue.Number,
			nil,
		)
		if err != nil {
			return errors.Wrapf(err, "failed to list labels for Issue %s", event.Issue.GetHTMLURL())
		}

		if labelsContainsLabel(labels, "ignore/qe") {
			logger.Debug("'ignore/qe' label present, ignoring event on " + number)
			return nil
		}

		if _, ok := postProcessing[number]; ok {
			logger.Debug("Post process issue: " + number)
			//delete(postProcessing, number)
			go postProcess(event, gh, config, logger)
			return nil
		} else {
			clearProgressLabel(*event.GetIssue(), gh, event.Repo)
		}

	} else if "issues_reopened" == eventKey && event.GetIssue().GetLocked() {
		// move
		err := moveIssueOnBoard(config, number, *doneColumn, logger)

		if err != nil {
			logger.Error("Post processing failed: Cannot move issue")
		} else {

			if _, ok := postProcessing[number]; ok {
				logger.Debug("Clear post processing for issue: " + number)
				delete(postProcessing, number)
			}

			// update progress/* label
			changeProgressLabel(gh, event.Repo, *event.Issue, doneColumn.name)

			response, err := gh.Issues.Unlock(context.Background(), event.Repo.Owner.GetLogin(), event.Repo.GetName(), *event.Issue.Number)

			if err != nil {
				logger.Warn("Error unlocking issue: " + response.Status)
			}

		}
	} else if "issues_opened" == eventKey && event.GetIssue().GetMilestone() != nil {

		// check if milestoned event is configured
		_, ok := stateMapping["issues_milestoned"]
		if ok {
			logger.Debug("Issue carries milestone, ignore event")
			return nil
		}
	} else if "issues_milestoned" == eventKey {
		// only move from inbox forward
		err, col := getIssueColumn(config, number, logger)
		if err != nil {
			logger.Error("Error retrieving issue column", zap.Error(err))
		}

		if col != inboxColumn.name {
			logger.Debug("Milestone event for issue outside the Inbox, not moving  #" + number)
			return nil
		}

	} else if "issues_demilestoned" == eventKey {
		logger.Info("Ignore  issues_demilestoned event.")
		return nil
	}

	// cleanup post processing markers, but skip actions
	if event.GetIssue().GetLocked() == true {
		logger.Debug("Ignore event for locked issue: " + number)
		return nil
	}

	// regular processing
	col, ok := stateMapping[eventKey]
	if ok {
		err := moveIssueOnBoard(config, number, col, logger)

		if nil == err {
			// update progress/* label
			changeProgressLabel(gh, event.Repo, *event.Issue, col.name)
		}

		return err
	} else {
		logger.Debug("Ignore unmapped Issue event: " + eventKey)
	}

	return nil
}

func changeProgressLabel(gh *github.Client, repo *github.Repository, issue github.Issue, newLabel string) {

	/*clearProgressLabel(issue, gh, repo)

	labels := []string{"progress/" + newLabel}

	gh.Issues.AddLabelsToIssue(context.Background(), repo.Owner.GetLogin(), repo.GetName(),
		issue.GetNumber(), labels)*/

}

func clearProgressLabel(issue github.Issue, gh *github.Client, repo *github.Repository) {
	for _, label := range issue.Labels {
		if strings.HasPrefix(*label.Name, "progress/") {
			gh.Issues.RemoveLabelForIssue(context.Background(), repo.Owner.GetLogin(), repo.GetName(),
				issue.GetNumber(), *label.Name)

		}
	}
}

func postProcess(event *github.IssuesEvent, gh *github.Client, config config.RepoConfig, logger *zap.Logger) {

	number := strconv.Itoa(*event.Issue.Number)

	logger.Debug("Enter grace time before prost processing ... ")
	time.Sleep(10 * time.Second)

	_, e := gh.Issues.Lock(context.Background(), event.Repo.Owner.GetLogin(), event.Repo.GetName(), *event.Issue.Number, &github.LockIssueOptions{LockReason: "resolved"})
	if e != nil {
		logger.Error("Locking issue failed: " + number)
	}

	// re-open
	state := "open"
	_, _, err := gh.Issues.Edit(context.Background(), event.Repo.Owner.GetLogin(), event.Repo.GetName(), *event.Issue.Number, &github.IssueRequest{State: &state})
	if err != nil {
		logger.Error("Post processing failed ")
	}

}

func (h *boardUpdate) handlePullRequestEvent(event *github.PullRequestEvent, gh *github.Client, config config.RepoConfig, logger *zap.Logger) error {

	var messageType = "pull_request"
	eventKey := messageType + "_" + *event.Action

	prNumber := strconv.Itoa(*event.PullRequest.Number)
	logger.Info("<< Event " + eventKey + " on PR " + prNumber + " >>")

	commits, _, err := gh.PullRequests.ListCommits(context.Background(), event.Repo.Owner.GetLogin(), event.Repo.GetName(),
		*event.PullRequest.Number, nil)

	if err != nil {
		logger.Error("Failed to retrieve commits")
		return nil
	}

	issues := []string{}

	// find issues in commit messages
	for _, commit := range commits {
		message := *commit.Commit.Message
		match := regex.Match([]byte(message))
		logger.Debug("keyword in commit message? " + strconv.FormatBool(match))
		extractIssueNumbers(&issues, message)
	}

	// find issues in PR message
	if len(issues) == 0 {
		prMessage := event.GetPullRequest().GetBody()
		match2 := regex.Match([]byte(prMessage))
		logger.Debug("keyword in PR message? " + strconv.FormatBool(match2))
		extractIssueNumbers(&issues, prMessage)
	}

	logger.Debug("number issues references found: " + strconv.Itoa(len(issues)))

	// process issues
	for _, number := range issues {

		if _, ok := postProcessing[number]; ok {
			logger.Debug("Issue scheduled for post processing, ignore event for issue: " + number)
			continue
		}

		// schedule post processing if needed
		if "pull_request_opened" == eventKey &&
			doneColumn.isPostMergePipeline {

			// schedule completion with next event
			logger.Debug("Schedule post processing for issue: " + number)
			postProcessing[number] = *doneColumn
			continue
		}

		// regular PR processing
		col, ok := stateMapping[eventKey]
		if ok {
			err := moveIssueOnBoard(config, number, col, logger)

			i, _ := strconv.Atoi(number)
			item, _, _ := gh.Issues.Get(context.Background(), event.Repo.Owner.GetLogin(), event.Repo.GetName(), i)

			if nil == err {
				changeProgressLabel(gh, event.Repo, *item, col.name)
			}
			return err
		} else {
			logger.Debug("Ignore unmapped PR event: " + eventKey)
		}
	}

	return nil
}

func extractIssueNumbers(issues *[]string, commitMessage string) {
	groupNames := regex.SubexpNames()

	for _, match := range regex.FindAllStringSubmatch(commitMessage, -1) {
		for groupIdx, _ := range match {
			name := groupNames[groupIdx]

			if name == "issue" {
				*issues = append(*issues, match[1])
			}
		}
	}
}

func moveIssueOnBoard(config config.RepoConfig, issue string, col column, logger *zap.Logger) error {

	logger.Info("Moving #" + issue + " to `" + col.name + "`")

	url := zenHubApi + "/p1/repositories/" + config.Board.GithubRepo + "/issues/" + issue + "/moves"
	response, err := resty.R().
		SetHeader("X-Authentication-Token", config.Board.ZenhubToken).
		SetHeader("Content-Type", "application/json").
		SetBody(`{"pipeline_id":"` + col.id + `", "position": "top"}`).
		Post(url)

	logger.Debug("Zenhub call status: HTTP " + strconv.Itoa(response.StatusCode()) + " from " + url)

	if err != nil {
		return err
	}

	if response.StatusCode() > 400 {
		logger.Warn("Zenhub call unsuccessful: HTTP " + strconv.Itoa(response.StatusCode()) + " from " + url)
	}

	return nil
}

func getIssueColumn(config config.RepoConfig, issue string, logger *zap.Logger) (error, string) {

	url := zenHubApi + "/p1/repositories/" + config.Board.GithubRepo + "/issues/" + issue
	response, err := resty.R().
		SetHeader("X-Authentication-Token", config.Board.ZenhubToken).
		SetHeader("Content-Type", "application/json").
		Get(url)

	if err != nil {
		return err, ""
	}

	if response.StatusCode() > 400 {
		logger.Warn("Zenhub call unsuccessful: HTTP " + strconv.Itoa(response.StatusCode()) + " from " + url)
	}

	bytes := response.Body()[:]

	var zenhubIssue ZenhubIssue
	err = json.Unmarshal(bytes, &zenhubIssue)

	if err != nil {
		return err, ""
	}

	return nil, zenhubIssue.Pipeline.Name
}
