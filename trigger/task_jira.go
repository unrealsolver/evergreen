package trigger

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"text/template"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/model"
	"github.com/evergreen-ci/evergreen/model/build"
	"github.com/evergreen-ci/evergreen/model/event"
	"github.com/evergreen-ci/evergreen/model/host"
	"github.com/evergreen-ci/evergreen/model/task"
	"github.com/evergreen-ci/evergreen/model/version"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/message"
	"github.com/pkg/errors"
)

// DescriptionTemplateString defines the content of the alert ticket.
const descriptionTemplateString = `
h2. [{{.Task.DisplayName}} failed on {{.Build.DisplayName}}|{{taskurl .}}]
Host: {{host .}}
Project: [{{.Project.DisplayName}}|{{.UIRoot}}/waterfall/{{.Project.Identifier}}]
Commit: [diff|https://github.com/{{.Project.Owner}}/{{.Project.Repo}}/commit/{{.Version.Revision}}]: {{.Version.Message}}
Evergreen Subscription: {{.SubscriptionID}}; Evergreen Event: {{.EventID}}
{{range .Tests}}*{{.Name}}* - [Logs|{{.URL}}] | [History|{{.HistoryURL}}]
{{end}}
`
const (
	jiraMaxTitleLength = 254

	failedTestNamesTmpl = "%%FailedTestNames%%"
)

// descriptionTemplate is filled to create a JIRA alert ticket. Panics at start if invalid.
var descriptionTemplate = template.Must(template.New("Desc").Funcs(template.FuncMap{
	"taskurl": getTaskURL,
	"host":    getHostMetadata,
}).Parse(descriptionTemplateString))

func getHostMetadata(data *jiraTemplateData) string {
	if data.Host == nil {
		return "N/A"
	}

	return fmt.Sprintf("[%s|%s/host/%s]", data.Host.Host, data.UIRoot, url.PathEscape(data.Host.Id))
}

func getTaskURL(data *jiraTemplateData) (string, error) {
	if data.Task == nil {
		return "", errors.New("task is nil")
	}
	id := data.Task.Id
	execution := data.Task.Execution
	if data.Task.DisplayTask != nil {
		id = data.Task.DisplayTask.Id
		execution = data.Task.DisplayTask.Execution
	} else if len(data.Task.OldTaskId) != 0 {
		id = data.Task.OldTaskId
	}

	return taskLink(data.UIRoot, id, execution), nil
}

// jiraTestFailure contains the required fields for generating a failure report.
type jiraTestFailure struct {
	Name       string
	URL        string
	HistoryURL string
}

type jiraBuilder struct {
	project   string
	issueType string
	mappings  *evergreen.JIRANotificationsConfig

	data jiraTemplateData
}

type jiraTemplateData struct {
	UIRoot             string
	SubscriptionID     string
	EventID            string
	Task               *task.Task
	Build              *build.Build
	Host               *host.Host
	Project            *model.ProjectRef
	Version            *version.Version
	FailedTests        []task.TestResult
	FailedTestNames    []string
	Tests              []jiraTestFailure
	SpecificTaskStatus string
}

func makeSpecificTaskStatus(t *task.Task) string {
	switch {
	case t.Status == evergreen.TaskSucceeded:
		return evergreen.TaskSucceeded
	case t.Details.TimedOut:
		return evergreen.TaskTimedOut
	case t.Details.Type == evergreen.CommandTypeSystem:
		return evergreen.TaskSystemFailed
	case t.Details.Type == evergreen.CommandTypeSetup:
		return evergreen.TaskSetupFailed
	default:
		return evergreen.TaskFailed
	}
}

func makeSummaryPrefix(t *task.Task, failed int) string {
	s := makeSpecificTaskStatus(t)
	switch {
	case s == evergreen.TaskSucceeded:
		return "Succeeded: "
	case s == evergreen.TaskTimedOut:
		return "Timed Out: "
	case s == evergreen.TaskSystemFailed:
		return "System Failure: "
	case s == evergreen.TaskSetupFailed:
		return "Setup Failure: "
	case failed == 1:
		return "Failure: "
	case failed > 1:
		return "Failures: "
	default:
		return "Failed: "
	}
}

func (j *jiraBuilder) build() (*message.JiraIssue, error) {
	j.data.SpecificTaskStatus = makeSpecificTaskStatus(j.data.Task)
	description, err := j.getDescription()
	if err != nil {
		return nil, errors.Wrap(err, "error creating description")
	}
	summary, err := j.getSummary()
	if err != nil {
		return nil, errors.Wrap(err, "error creating summary")
	}

	issue := message.JiraIssue{
		Project:     j.project,
		Type:        j.issueType,
		Summary:     summary,
		Description: description,
		Fields:      j.makeCustomFields(),
	}

	if err != nil {
		return nil, errors.Wrap(err, "error creating description")
	}
	grip.Info(message.Fields{
		"message":      "creating jira ticket for failure",
		"type":         j.issueType,
		"jira_project": j.project,
		"task":         j.data.Task.Id,
		"project":      j.data.Project.Identifier,
	})

	event.LogJiraIssueCreated(j.data.Task.Id, j.data.Task.Execution, j.project)

	return &issue, nil
}

// getSummary creates a JIRA subject for a task failure in the style of
//  Failures: Task_name on Variant (test1, test2) [ProjectName @ githash]
// based on the given AlertContext.
func (j *jiraBuilder) getSummary() (string, error) {
	subj := &bytes.Buffer{}
	failed := []string{}

	for _, test := range j.data.Task.LocalTestResults {
		if test.Status == evergreen.TestFailedStatus {
			failed = append(failed, cleanTestName(test.TestFile))
		}
	}

	subj.WriteString(makeSummaryPrefix(j.data.Task, len(failed)))

	catcher := grip.NewSimpleCatcher()
	if j.data.Task.DisplayTask != nil {
		_, err := fmt.Fprintf(subj, j.data.Task.DisplayTask.DisplayName)
		catcher.Add(err)
	} else {
		_, err := fmt.Fprintf(subj, j.data.Task.DisplayName)
		catcher.Add(err)
	}
	_, err := fmt.Fprintf(subj, " on %s ", j.data.Build.DisplayName)
	catcher.Add(err)
	_, err = fmt.Fprintf(subj, "[%s @ %s] ", j.data.Project.DisplayName, j.data.Version.Revision[0:8])
	catcher.Add(err)

	if len(failed) > 0 {
		// Include an additional 10 characters for overhead, like the
		// parens and number of failures.
		remaining := jiraMaxTitleLength - subj.Len() - 10

		if remaining < len(failed[0]) {
			return subj.String(), catcher.Resolve()
		}
		subj.WriteString("(")
		toPrint := []string{}
		for _, fail := range failed {
			if remaining-len(fail) > 0 {
				toPrint = append(toPrint, fail)
			}
			remaining = remaining - len(fail) - 2
		}
		_, err = fmt.Fprint(subj, strings.Join(toPrint, ", "))
		catcher.Add(err)
		if len(failed)-len(toPrint) > 0 {
			_, err := fmt.Fprintf(subj, " +%d more", len(failed)-len(toPrint))
			catcher.Add(err)
		}
		subj.WriteString(")")
	}
	// Truncate string in case we made some mistake above, since it's better
	// to have a truncated title than to miss a Jira ticket.
	if subj.Len() > jiraMaxTitleLength {
		return subj.String()[:jiraMaxTitleLength], catcher.Resolve()
	}
	return subj.String(), catcher.Resolve()
}

func (j *jiraBuilder) makeCustomFields() map[string]interface{} {
	fields := map[string]interface{}{}
	m, err := j.mappings.CustomFields.ToMap()
	if err != nil {
		grip.Error(message.WrapError(err, message.Fields{
			"message": "failed to build custom fields",
			"task_id": j.data.Task.Id,
		}))
		return nil
	}
	customFields, ok := m[j.project]
	if !ok || len(customFields) == 0 {
		return nil
	}

	for i := range j.data.Task.LocalTestResults {
		if j.data.Task.LocalTestResults[i].Status == evergreen.TestFailedStatus {
			j.data.FailedTests = append(j.data.FailedTests, j.data.Task.LocalTestResults[i])
			j.data.FailedTestNames = append(j.data.FailedTestNames, j.data.Task.LocalTestResults[i].TestFile)
		}
	}

	for fieldName, fieldTmpl := range customFields {
		if fieldTmpl == failedTestNamesTmpl {
			fields[fieldName] = j.data.FailedTestNames
			continue
		}

		tmpl, err := template.New(fmt.Sprintf("%s-%s", j.project, fieldName)).Parse(fieldTmpl)
		if err != nil {
			// Admins should be notified of misconfiguration, but we shouldn't block
			// ticket generation
			grip.Alert(message.WrapError(err, message.Fields{
				"message":      "invalid custom field template",
				"jira_project": j.project,
				"jira_field":   fieldName,
				"template":     fieldTmpl,
			}))
			continue
		}

		buf := &bytes.Buffer{}
		if err = tmpl.Execute(buf, &j.data); err != nil {
			grip.Alert(message.WrapError(err, message.Fields{
				"message":      "template execution failed",
				"jira_project": j.project,
				"jira_field":   fieldName,
				"template":     fieldTmpl,
			}))
			continue
		}

		fields[fieldName] = []string{buf.String()}
	}
	return fields
}

// historyURL provides a full URL to the test's task history page.
func historyURL(t *task.Task, testName, uiRoot string) string {
	return fmt.Sprintf("%v/task_history/%v/%v#%v=fail",
		uiRoot, url.PathEscape(t.Project), url.PathEscape(t.Id), url.QueryEscape(testName))
}

// logURL returns the full URL for linking to a test's logs.
// Returns the empty string if no internal or external log is referenced.
func logURL(test task.TestResult, root string) string {
	if test.LogId != "" {
		return root + "/test_log/" + url.PathEscape(test.LogId)
	}
	return test.URL
}

// getDescription returns the body of the JIRA ticket, with links.
func (j *jiraBuilder) getDescription() (string, error) {
	const jiraMaxDescLength = 32767
	// build a list of all failed tests to include
	tests := []jiraTestFailure{}
	for _, test := range j.data.Task.LocalTestResults {
		if test.Status == evergreen.TestFailedStatus {
			tests = append(tests, jiraTestFailure{
				Name:       cleanTestName(test.TestFile),
				URL:        logURL(test, j.data.UIRoot),
				HistoryURL: historyURL(j.data.Task, cleanTestName(test.TestFile), j.data.UIRoot),
			})
		}
	}

	buf := &bytes.Buffer{}
	j.data.Tests = tests
	if err := descriptionTemplate.Execute(buf, &j.data); err != nil {
		return "", err
	}
	// Jira description length maximum
	if buf.Len() > jiraMaxDescLength {
		buf.Truncate(jiraMaxDescLength)
	}
	return buf.String(), nil
}

// cleanTestName returns the last item of a test's path.
//   TODO: stop accommodating this.
func cleanTestName(path string) string {
	if unixIdx := strings.LastIndex(path, "/"); unixIdx != -1 {
		// if the path ends in a slash, remove it and try again
		if unixIdx == len(path)-1 {
			return cleanTestName(path[:len(path)-1])
		}
		return path[unixIdx+1:]
	}
	if windowsIdx := strings.LastIndex(path, `\`); windowsIdx != -1 {
		// if the path ends in a slash, remove it and try again
		if windowsIdx == len(path)-1 {
			return cleanTestName(path[:len(path)-1])
		}
		return path[windowsIdx+1:]
	}
	return path
}
