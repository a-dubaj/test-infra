/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mungers

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/test-infra/mungegithub/features"
	"k8s.io/test-infra/mungegithub/github"
	"k8s.io/test-infra/mungegithub/mungers/approvers"
	c "k8s.io/test-infra/mungegithub/mungers/matchers/comment"
	"k8s.io/test-infra/mungegithub/mungers/matchers/event"
	"k8s.io/test-infra/mungegithub/mungers/mungerutil"
	"k8s.io/test-infra/mungegithub/options"

	githubapi "github.com/google/go-github/github"
)

type milestoneState int

type milestoneOptName string

// milestoneStateConfig defines the label and notification
// configuration for a given milestone state.
type milestoneStateConfig struct {
	// The milestone label to apply to the label (all other milestone state labels will be removed)
	label string
	// The title of the notification message
	title string
	// Whether the notification should be repeated on the configured interval
	warnOnInterval bool
	// Whether sigs should be mentioned in the notification message
	notifySIGs bool
}

const (
	milestoneNotifierName = "MilestoneNotifier"

	milestoneModeDev    = "dev"
	milestoneModeSlush  = "slush"
	milestoneModeFreeze = "freeze"

	milestoneCurrent        milestoneState = iota // No change is required.
	milestoneNeedsLabeling                        // One or more priority/*, kind/* and sig/* labels are missing.
	milestoneNeedsApproval                        // The status/needs-approval label is missing.
	milestoneNeedsAttention                       // A status/* label is missing or an update is required.
	milestoneNeedsRemoval                         // The issue needs to be removed from the milestone.

	milestoneLabelsIncompleteLabel = "milestone/incomplete-labels"
	milestoneNeedsApprovalLabel    = "milestone/needs-approval"
	milestoneNeedsAttentionLabel   = "milestone/needs-attention"
	milestoneRemovedLabel          = "milestone/removed"

	statusApprovedLabel   = "status/approved-for-milestone"
	statusInProgressLabel = "status/in-progress"

	blockerLabel = "priority/critical-urgent"

	sigLabelPrefix     = "sig/"
	sigMentionTemplate = "@kubernetes/sig-%s-bugs"

	milestoneOptActiveMilestone      = "active-milestone"
	milestoneOptMode                 = "milestone-mode"
	milestoneOptWarningInterval      = "milestone-warning-interval"
	milestoneOptLabelGracePeriod     = "milestone-label-grace-period"
	milestoneOptApprovalGracePeriod  = "milestone-approval-grace-period"
	milestoneOptSlushUpdateInterval  = "milestone-slush-update-interval"
	milestoneOptFreezeUpdateInterval = "milestone-freeze-update-interval"
	milestoneOptFreezeDate           = "milestone-freeze-date"

	milestoneDetail = `<details>
<summary>Help</summary>
<ul>
 <li><a href="https://github.com/kubernetes/community/blob/master/contributors/devel/release/issues.md">Additional instructions</a></li>
 <li><a href="https://github.com/kubernetes/test-infra/blob/master/commands.md">Commands for setting labels</a></li>
</ul>
</details>
`

	milestoneMessageTemplate = `
{{- if .warnUnapproved}}
**Action required**: This issue must have the {{.approvedLabel}} label applied by a SIG maintainer.{{.unapprovedRemovalWarning}}
{{end -}}
{{- if .removeUnapproved}}
**Important**: This issue was missing the {{.approvedLabel}} label for more than {{.approvalGracePeriod}}.
{{end -}}
{{- if .warnMissingInProgress}}
**Action required**: During code {{.mode}}, issues in the milestone should be in progress.
If this issue is not being actively worked on, please remove it from the milestone.
If it is being worked on, please add the {{.inProgressLabel}} label so it can be tracked with other in-flight issues.
{{end -}}
{{- if .warnUpdateRequired}}
**Action Required**: This issue has not been updated since {{.lastUpdated}}. Please provide an update.
{{end -}}
{{- if .warnUpdateInterval}}
**Note**: This issue is marked as {{.blockerLabel}}, and must be updated every {{.updateInterval}} during code {{.mode}}.

Example update:

` + "```" + `
ACK.  In progress
ETA: DD/MM/YYYY
Risks: Complicated fix required
` + "```" + `
{{end -}}
{{- if .warnNonBlockerRemoval}}
**Note**: If this issue is not resolved or labeled as {{.blockerLabel}} by {{.freezeDate}} it will be moved out of the {{.milestone}}.
{{end -}}
{{- if .removeNonBlocker}}
**Important**: Code freeze is in effect and only issues with {{.blockerLabel}} may remain in the {{.milestone}}.
{{end -}}
{{- if .warnIncompleteLabels}}
**Action required**: This issue requires label changes.{{.incompleteLabelsRemovalWarning}}

{{range $index, $labelError := .labelErrors -}}
{{$labelError}}
{{end -}}
{{end -}}
{{- if .removeIncompleteLabels}}
**Important**: This issue was missing labels required for the {{.milestone}} for more than {{.labelGracePeriod}}:

{{range $index, $labelError := .labelErrors -}}
{{$labelError}}
{{end}}
{{end -}}
{{- if .summarizeLabels -}}
<details{{if .onlySummary}} open{{end}}>
<summary>Issue Labels</summary>

- {{range $index, $sigLabel := .sigLabels}}{{if $index}} {{end}}{{$sigLabel}}{{end}}: Issue will be escalated to these SIGs if needed.
- {{.priorityLabel}}: {{.priorityDescription}}
- {{.kindLabel}}: {{.kindDescription}}
</details>
{{- end -}}
`
)

var (
	milestoneModes = sets.NewString(milestoneModeDev, milestoneModeSlush, milestoneModeFreeze)

	milestoneStateConfigs = map[milestoneState]milestoneStateConfig{
		milestoneCurrent: {
			title: "Milestone Issue **Current**",
		},
		milestoneNeedsLabeling: {
			title:          "Milestone Labels **Incomplete**",
			label:          milestoneLabelsIncompleteLabel,
			warnOnInterval: true,
		},
		milestoneNeedsApproval: {
			title:          "Milestone Issue **Needs Approval**",
			label:          milestoneNeedsApprovalLabel,
			warnOnInterval: true,
			notifySIGs:     true,
		},
		milestoneNeedsAttention: {
			title:          "Milestone Issue **Needs Attention**",
			label:          milestoneNeedsAttentionLabel,
			warnOnInterval: true,
			notifySIGs:     true,
		},
		milestoneNeedsRemoval: {
			title:      "Milestone **Removed**",
			label:      milestoneRemovedLabel,
			notifySIGs: true,
		},
	}

	// milestoneStateLabels is the set of milestone labels applied by
	// the munger.  statusApprovedLabel is not included because it is
	// applied manually rather than by the munger.
	milestoneStateLabels = []string{
		milestoneLabelsIncompleteLabel,
		milestoneNeedsApprovalLabel,
		milestoneNeedsAttentionLabel,
		milestoneRemovedLabel,
	}

	kindMap = map[string]string{
		"kind/bug":     "Fixes a bug discovered during the current release.",
		"kind/feature": "New functionality.",
		"kind/cleanup": "Adding tests, refactoring, fixing old bugs.",
	}

	priorityMap = map[string]string{
		blockerLabel:                  "Never automatically move out of a release milestone; continually escalate to contributor and SIG through all available channels.",
		"priority/important-soon":     "Escalate to the issue owners and SIG owner; move out of milestone after several unsuccessful escalation attempts.",
		"priority/important-longterm": "Escalate to the issue owners; move out of the milestone after 1 attempt.",
	}
)

// issueChange encapsulates changes to make to an issue.
type issueChange struct {
	notification        *c.Notification
	label               string
	commentInterval     *time.Duration
	removeFromMilestone bool
}

type milestoneArgValidator func(name string) error

// MilestoneMaintainer enforces the process for shepherding issues into the release.
type MilestoneMaintainer struct {
	botName    string
	features   *features.Features
	validators map[string]milestoneArgValidator

	activeMilestone      string
	mode                 string
	warningInterval      time.Duration
	labelGracePeriod     time.Duration
	approvalGracePeriod  time.Duration
	slushUpdateInterval  time.Duration
	freezeUpdateInterval time.Duration
	freezeDate           string
}

func init() {
	RegisterMungerOrDie(NewMilestoneMaintainer())
}

func NewMilestoneMaintainer() *MilestoneMaintainer {
	m := &MilestoneMaintainer{}
	m.validators = map[string]milestoneArgValidator{
		milestoneOptActiveMilestone: func(name string) error {
			if len(m.activeMilestone) == 0 {
				return fmt.Errorf("%s must be supplied", name)
			}
			return nil
		},
		milestoneOptMode: func(name string) error {
			if !milestoneModes.Has(m.mode) {
				return fmt.Errorf("%s must be one of %v", name, milestoneModes.List())
			}
			return nil
		},
		milestoneOptWarningInterval: func(name string) error {
			return durationGreaterThanZero(name, m.warningInterval)
		},
		milestoneOptLabelGracePeriod: func(name string) error {
			return durationGreaterThanZero(name, m.labelGracePeriod)
		},
		milestoneOptApprovalGracePeriod: func(name string) error {
			return durationGreaterThanZero(name, m.approvalGracePeriod)
		},
		milestoneOptSlushUpdateInterval: func(name string) error {
			return durationGreaterThanZero(name, m.slushUpdateInterval)
		},
		milestoneOptFreezeUpdateInterval: func(name string) error {
			return durationGreaterThanZero(name, m.freezeUpdateInterval)
		},
		milestoneOptFreezeDate: func(name string) error {
			if m.mode == milestoneModeSlush && len(m.freezeDate) == 0 {
				return fmt.Errorf("%s must be supplied when milestone-mode is 'slush'", name)
			}
			return nil
		},
	}
	return m
}
func durationGreaterThanZero(name string, value time.Duration) error {
	if value <= 0 {
		return fmt.Errorf("%s must be greater than zero", name)
	}
	return nil
}

// Name is the name usable in --pr-mungers
func (m *MilestoneMaintainer) Name() string { return "milestone-maintainer" }

// RequiredFeatures is a slice of 'features' that must be provided
func (m *MilestoneMaintainer) RequiredFeatures() []string { return []string{} }

// Initialize will initialize the munger
func (m *MilestoneMaintainer) Initialize(config *github.Config, features *features.Features) error {
	for name, validator := range m.validators {
		if err := validator(name); err != nil {
			return err
		}
	}

	m.botName = config.BotName
	m.features = features
	return nil
}

// EachLoop is called at the start of every munge loop. This function
// is a no-op for the munger because to munge an issue it only needs
// the state local to the issue.
func (m *MilestoneMaintainer) EachLoop() error { return nil }

// RegisterOptions registers options for this munger; returns any that require a restart when changed.
func (m *MilestoneMaintainer) RegisterOptions(opts *options.Options) sets.String {
	opts.RegisterString(&m.activeMilestone, milestoneOptActiveMilestone, "", "The active milestone that this munger will maintain issues for.")
	opts.RegisterString(&m.mode, milestoneOptMode, milestoneModeDev, fmt.Sprintf("The release cycle process to enforce.  Valid values are %v.", milestoneModes.List()))
	opts.RegisterDuration(&m.warningInterval, milestoneOptWarningInterval, 24*time.Hour, "The interval to wait between warning about an incomplete issue in the active milestone.")
	opts.RegisterDuration(&m.labelGracePeriod, milestoneOptLabelGracePeriod, 72*time.Hour, "The grace period to wait before removing a non-blocking issue with incomplete labels from the active milestone.")
	opts.RegisterDuration(&m.approvalGracePeriod, milestoneOptApprovalGracePeriod, 168*time.Hour, "The grace period to wait before removing a non-blocking issue without sig approval from the active milestone.")
	opts.RegisterDuration(&m.slushUpdateInterval, milestoneOptSlushUpdateInterval, 72*time.Hour, "The expected interval, during code slush, between updates to a blocking issue in the active milestone.")
	opts.RegisterDuration(&m.freezeUpdateInterval, milestoneOptFreezeUpdateInterval, 24*time.Hour, "The expected interval, during code freeze, between updates to a blocking issue in the active milestone.")
	opts.RegisterString(&m.freezeDate, milestoneOptFreezeDate, "", fmt.Sprintf("The date string indicating when code freeze will take effect."))
	opts.RegisterUpdateCallback(func(changed sets.String) error {
		for name, validator := range m.validators {
			if changed.Has(name) {
				if err := validator(name); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return nil
}

func (m *MilestoneMaintainer) updateInterval() time.Duration {
	if m.mode == milestoneModeSlush {
		return m.slushUpdateInterval
	}
	if m.mode == milestoneModeFreeze {
		return m.freezeUpdateInterval
	}
	return 0
}

// Munge is the workhorse the will actually make updates to the issue
func (m *MilestoneMaintainer) Munge(obj *github.MungeObject) {
	if ignoreObject(obj, m.activeMilestone) {
		return
	}

	change := m.issueChange(obj)
	if change == nil {
		return
	}

	if !updateMilestoneStateLabel(obj, change.label) {
		return
	}

	comment, ok := latestNotificationComment(obj, m.botName)
	if !ok {
		return
	}
	if !notificationIsCurrent(change.notification, comment, change.commentInterval) {
		if comment != nil {
			if err := obj.DeleteComment(comment.Source.(*githubapi.IssueComment)); err != nil {
				return
			}
		}
		if err := change.notification.Post(obj); err != nil {
			return
		}
	}

	if change.removeFromMilestone {
		obj.ClearMilestone()
	}
}

// issueChange computes the changes required to modify the state of
// the issue to reflect the milestone process. If a nil return value
// is returned, no action should be taken.
func (m *MilestoneMaintainer) issueChange(obj *github.MungeObject) *issueChange {
	icc := m.issueChangeConfig(obj)
	if icc == nil {
		return nil
	}

	messageBody := icc.messageBody()
	if messageBody == nil {
		return nil
	}

	stateConfig := milestoneStateConfigs[icc.state]

	mentions := mungerutil.GetIssueUsers(obj.Issue).AllUsers().Mention().Join()
	if stateConfig.notifySIGs {
		sigMentions := icc.sigMentions()
		if len(sigMentions) > 0 {
			mentions = fmt.Sprintf("%s %s", mentions, sigMentions)
		}
	}

	message := fmt.Sprintf("%s\n\n%s\n%s", mentions, *messageBody, milestoneDetail)

	var commentInterval *time.Duration
	if stateConfig.warnOnInterval {
		commentInterval = &m.warningInterval
	}

	return &issueChange{
		notification:        c.NewNotification(milestoneNotifierName, stateConfig.title, message),
		label:               stateConfig.label,
		removeFromMilestone: icc.state == milestoneNeedsRemoval,
		commentInterval:     commentInterval,
	}
}

// issueChangeConfig computes the configuration required to determine
// the changes to make to an issue so that it reflects the milestone
// process. If a nil return value is returned, no action should be
// taken.
func (m *MilestoneMaintainer) issueChangeConfig(obj *github.MungeObject) *issueChangeConfig {
	updateInterval := m.updateInterval()

	icc := &issueChangeConfig{
		enabledSections: sets.String{},
		templateArguments: map[string]interface{}{
			"approvalGracePeriod": durationToMaxDays(m.approvalGracePeriod),
			"approvedLabel":       quoteLabel(statusApprovedLabel),
			"blockerLabel":        quoteLabel(blockerLabel),
			"freezeDate":          m.freezeDate,
			"inProgressLabel":     quoteLabel(statusInProgressLabel),
			"labelGracePeriod":    durationToMaxDays(m.labelGracePeriod),
			"milestone":           fmt.Sprintf("%s milestone", m.activeMilestone),
			"mode":                m.mode,
			"updateInterval":      durationToMaxDays(updateInterval),
		},
		sigLabels: []string{},
	}

	isBlocker := obj.HasLabel(blockerLabel)

	if kind, priority, sigs, labelErrors := checkLabels(obj.Issue.Labels); len(labelErrors) == 0 {
		icc.summarizeLabels(kind, priority, sigs)
		if !obj.HasLabel(statusApprovedLabel) {
			if isBlocker {
				icc.warnUnapproved(nil, m.activeMilestone)
			} else {
				removeAfter, ok := gracePeriodRemaining(obj, m.botName, milestoneNeedsApprovalLabel, m.approvalGracePeriod, time.Now(), false)
				if !ok {
					return nil
				}

				if removeAfter == nil || *removeAfter >= 0 {
					icc.warnUnapproved(removeAfter, m.activeMilestone)
				} else {
					icc.removeUnapproved()
				}
			}
			return icc
		}

		if m.mode == milestoneModeDev {
			// Status and updates are not required for dev mode
			return icc
		}

		if m.mode == milestoneModeFreeze && !isBlocker {
			icc.removeNonBlocker()
			return icc
		}

		if !obj.HasLabel(statusInProgressLabel) {
			icc.warnMissingInProgress()
		}

		if !isBlocker {
			icc.enableSection("warnNonBlockerRemoval")
		} else if updateInterval > 0 {
			lastUpdateTime, ok := findLastModificationTime(obj)
			if !ok {
				return nil
			}

			durationSinceUpdate := time.Since(*lastUpdateTime)
			if durationSinceUpdate > updateInterval {
				icc.warnUpdateRequired(*lastUpdateTime)
			}
			icc.enableSection("warnUpdateInterval")
		}
	} else {
		removeAfter, ok := gracePeriodRemaining(obj, m.botName, milestoneLabelsIncompleteLabel, m.labelGracePeriod, time.Now(), isBlocker)
		if !ok {
			return nil
		}

		if removeAfter == nil || *removeAfter >= 0 {
			icc.warnIncompleteLabels(removeAfter, labelErrors, m.activeMilestone)
		} else {
			icc.removeIncompleteLabels(labelErrors)
		}
	}
	return icc
}

// issueChangeConfig is the config required to change an issue (via
// comments and labeling) to reflect the reuqirements of the milestone
// maintainer.
type issueChangeConfig struct {
	state             milestoneState
	enabledSections   sets.String
	sigLabels         []string
	templateArguments map[string]interface{}
}

func (icc *issueChangeConfig) messageBody() *string {
	for _, sectionName := range icc.enabledSections.List() {
		// If an issue will be removed from the milestone, suppress non-removal sections
		if icc.state != milestoneNeedsRemoval || strings.HasPrefix(sectionName, "remove") {
			icc.templateArguments[sectionName] = true
		}
	}

	icc.templateArguments["onlySummary"] = icc.state == milestoneCurrent

	return approvers.GenerateTemplateOrFail(milestoneMessageTemplate, "message", icc.templateArguments)
}

func (icc *issueChangeConfig) enableSection(sectionName string) {
	icc.enabledSections.Insert(sectionName)
}

func (icc *issueChangeConfig) summarizeLabels(kindLabel, priorityLabel string, sigLabels []string) {
	icc.enableSection("summarizeLabels")
	icc.state = milestoneCurrent
	icc.sigLabels = sigLabels
	quotedSigLabels := []string{}
	for _, sigLabel := range sigLabels {
		quotedSigLabels = append(quotedSigLabels, quoteLabel(sigLabel))
	}
	arguments := map[string]interface{}{
		"kindLabel":           quoteLabel(kindLabel),
		"kindDescription":     kindMap[kindLabel],
		"priorityLabel":       quoteLabel(priorityLabel),
		"priorityDescription": priorityMap[priorityLabel],
		"sigLabels":           quotedSigLabels,
	}
	for k, v := range arguments {
		icc.templateArguments[k] = v
	}
}

func (icc *issueChangeConfig) warnUnapproved(removeAfter *time.Duration, milestone string) {
	icc.enableSection("warnUnapproved")
	icc.state = milestoneNeedsApproval
	var warning string
	if removeAfter != nil {
		warning = fmt.Sprintf(" If the label is not applied within %s, the issue will be moved out of the %s milestone.",
			durationToMaxDays(*removeAfter), milestone)
	}
	icc.templateArguments["unapprovedRemovalWarning"] = warning

}

func (icc *issueChangeConfig) removeUnapproved() {
	icc.enableSection("removeUnapproved")
	icc.state = milestoneNeedsRemoval
}

func (icc *issueChangeConfig) removeNonBlocker() {
	icc.enableSection("removeNonBlocker")
	icc.state = milestoneNeedsRemoval
}

func (icc *issueChangeConfig) warnMissingInProgress() {
	icc.enableSection("warnMissingInProgress")
	icc.state = milestoneNeedsAttention
}

func (icc *issueChangeConfig) warnUpdateRequired(lastUpdated time.Time) {
	icc.enableSection("warnUpdateRequired")
	icc.state = milestoneNeedsAttention
	icc.templateArguments["lastUpdated"] = lastUpdated.Format("Jan 2")
}

func (icc *issueChangeConfig) warnIncompleteLabels(removeAfter *time.Duration, labelErrors []string, milestone string) {
	icc.enableSection("warnIncompleteLabels")
	icc.state = milestoneNeedsLabeling
	var warning string
	if removeAfter != nil {
		warning = fmt.Sprintf(" If the required changes are not made within %s, the issue will be moved out of the %s milestone.",
			durationToMaxDays(*removeAfter), milestone)
	}
	icc.templateArguments["incompleteLabelsRemovalWarning"] = warning
	icc.templateArguments["labelErrors"] = labelErrors
}

func (icc *issueChangeConfig) removeIncompleteLabels(labelErrors []string) {
	icc.enableSection("removeIncompleteLabels")
	icc.state = milestoneNeedsRemoval
	icc.templateArguments["labelErrors"] = labelErrors
}

func (icc *issueChangeConfig) sigMentions() string {
	mentions := []string{}
	for _, label := range icc.sigLabels {
		sig := strings.TrimPrefix(label, sigLabelPrefix)
		target := fmt.Sprintf(sigMentionTemplate, sig)
		mentions = append(mentions, target)
	}
	return strings.Join(mentions, " ")
}

// ignoreObject indicates whether the munger should ignore the given
// object. Only issues in the active milestone should be munged.
func ignoreObject(obj *github.MungeObject, activeMilestone string) bool {
	// Only target issues
	if obj.IsPR() {
		return true
	}

	// Ignore closed issues
	if obj.Issue.State != nil && *obj.Issue.State == "closed" {
		return true
	}

	// Only target issues with an assigned milestone
	milestone, ok := obj.ReleaseMilestone()
	if !ok || len(milestone) == 0 {
		return true
	}

	// Only target issues in the active milestone
	return milestone != activeMilestone
}

// latestNotificationComment returns the most recent notification
// comment posted by the munger.
//
// Since the munger is careful to remove existing comments before
// adding new ones, only a single notification comment should exist.
func latestNotificationComment(obj *github.MungeObject, botName string) (*c.Comment, bool) {
	issueComments, ok := obj.ListComments()
	if !ok {
		return nil, false
	}
	comments := c.FromIssueComments(issueComments)
	notificationMatcher := c.MungerNotificationName(milestoneNotifierName, botName)
	notifications := c.FilterComments(comments, notificationMatcher)
	return notifications.GetLast(), true
}

// notificationIsCurrent indicates whether the given notification
// matches the most recent notification comment and the comment
// interval - if provided - has not been exceeded.
func notificationIsCurrent(notification *c.Notification, comment *c.Comment, commentInterval *time.Duration) bool {
	oldNotification := c.ParseNotification(comment)
	notificationsEqual := oldNotification != nil && oldNotification.Equal(notification)
	return notificationsEqual && (commentInterval == nil || comment != nil && comment.CreatedAt != nil && time.Since(*comment.CreatedAt) < *commentInterval)
}

// gracePeriodRemaining returns the difference between the start of
// the grace period and the grace period interval. Returns nil the
// grace period start cannot be determined.
func gracePeriodRemaining(obj *github.MungeObject, botName, labelName string, gracePeriod time.Duration, defaultStart time.Time, isBlocker bool) (*time.Duration, bool) {
	if isBlocker {
		return nil, true
	}
	tempStart := gracePeriodStart(obj, botName, labelName, defaultStart)
	if tempStart == nil {
		return nil, false
	}
	start := *tempStart

	remaining := -time.Since(start.Add(gracePeriod))
	return &remaining, true
}

// gracePeriodStart determines when the grace period for the given
// object should start as is indicated by when the
// milestone-labels-incomplete label was last applied. If the label
// is not set, the default will be returned. nil will be returned if
// an error occurs while accessing the object's label events.
func gracePeriodStart(obj *github.MungeObject, botName, labelName string, defaultStart time.Time) *time.Time {
	if !obj.HasLabel(labelName) {
		return &defaultStart
	}

	return labelLastCreatedAt(obj, botName, labelName)
}

// labelLastCreatedAt returns the time at which the given label was
// last applied to the given github object. Returns nil if an error
// occurs during event retrieval or if the label has never been set.
func labelLastCreatedAt(obj *github.MungeObject, botName, labelName string) *time.Time {
	events, ok := obj.GetEvents()
	if !ok {
		return nil
	}

	labelMatcher := event.And([]event.Matcher{
		event.AddLabel{},
		event.LabelName(labelName),
		event.Actor(botName),
	})
	labelEvents := event.FilterEvents(events, labelMatcher)
	lastAdded := labelEvents.GetLast()
	if lastAdded != nil {
		return lastAdded.CreatedAt
	}
	return nil
}

// checkLabels validates that the given labels are consistent with the
// requirements for an issue remaining in its chosen milestone.
// Returns the values of required labels (if present) and a slice of
// errors (where labels are not correct).
func checkLabels(labels []githubapi.Label) (kindLabel, priorityLabel string, sigLabels []string, labelErrors []string) {
	labelErrors = []string{}
	var err error

	kindLabel, err = uniqueLabelName(labels, kindMap)
	if err != nil || len(kindLabel) == 0 {
		kindLabels := formatLabelString(kindMap)
		labelErrors = append(labelErrors, fmt.Sprintf("_**kind**_: Must specify exactly one of %s.", kindLabels))
	}

	priorityLabel, err = uniqueLabelName(labels, priorityMap)
	if err != nil || len(priorityLabel) == 0 {
		priorityLabels := formatLabelString(priorityMap)
		labelErrors = append(labelErrors, fmt.Sprintf("_**priority**_: Must specify exactly one of %s.", priorityLabels))
	}

	sigLabels = sigLabelNames(labels)
	if len(sigLabels) == 0 {
		labelErrors = append(labelErrors, fmt.Sprintf("_**sig owner**_: Must specify at least one label prefixed with `%s`.", sigLabelPrefix))
	}

	return
}

// uniqueLabelName determines which label of a set indicated by a map
// - if any - is present in the given slice of labels. Returns an
// error if the slice contains more than one label from the set.
func uniqueLabelName(labels []githubapi.Label, labelMap map[string]string) (string, error) {
	var labelName string
	for _, label := range labels {
		_, exists := labelMap[*label.Name]
		if exists {
			if len(labelName) == 0 {
				labelName = *label.Name
			} else {
				return "", errors.New("Found more than one matching label")
			}
		}
	}
	return labelName, nil
}

// sigLabelNames returns a slice of the 'sig/' prefixed labels set on the issue.
func sigLabelNames(labels []githubapi.Label) []string {
	labelNames := []string{}
	for _, label := range labels {
		if strings.HasPrefix(*label.Name, sigLabelPrefix) {
			labelNames = append(labelNames, *label.Name)
		}
	}
	return labelNames
}

// formatLabelString converts a map to a string in the format "`key-foo`, `key-bar`".
func formatLabelString(labelMap map[string]string) string {
	labelList := []string{}
	for k := range labelMap {
		labelList = append(labelList, quoteLabel(k))
	}
	sort.Strings(labelList)

	maxIndex := len(labelList) - 1
	if maxIndex == 0 {
		return labelList[0]
	}
	return strings.Join(labelList[0:maxIndex], ", ") + " or " + labelList[maxIndex]
}

// quoteLabel formats a label name as inline code in markdown (e.g. `labelName`)
func quoteLabel(label string) string {
	if len(label) > 0 {
		return fmt.Sprintf("`%s`", label)
	}
	return label
}

// updateMilestoneStateLabel ensures that the given milestone state
// label is the only state label set on the given issue.
func updateMilestoneStateLabel(obj *github.MungeObject, labelName string) bool {
	if len(labelName) > 0 && !obj.HasLabel(labelName) {
		if err := obj.AddLabel(labelName); err != nil {
			return false
		}
	}
	for _, stateLabel := range milestoneStateLabels {
		if stateLabel != labelName && obj.HasLabel(stateLabel) {
			if err := obj.RemoveLabel(stateLabel); err != nil {
				return false
			}
		}
	}
	return true
}
