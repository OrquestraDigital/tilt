package hud

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/gdamore/tcell"
	"github.com/rivo/tview"

	"github.com/tilt-dev/tilt/internal/hud/view"
	"github.com/tilt-dev/tilt/internal/rty"
	"github.com/tilt-dev/tilt/pkg/model"
	"github.com/tilt-dev/tilt/pkg/model/logstore"
)

// These widths are determined experimentally, to see what shows up in a typical UX.
const DeployCellMinWidth = 8
const BuildDurCellMinWidth = 7
const BuildStatusCellMinWidth = 8
const MaxInlineErrHeight = 6

type ResourceView struct {
	logReader   logstore.Reader
	res         view.Resource
	rv          view.ResourceViewState
	triggerMode model.TriggerMode
	selected    bool

	clock func() time.Time
}

func NewResourceView(logReader logstore.Reader, res view.Resource, rv view.ResourceViewState, triggerMode model.TriggerMode,
	selected bool, clock func() time.Time) *ResourceView {
	return &ResourceView{
		logReader:   logReader,
		res:         res,
		rv:          rv,
		triggerMode: triggerMode,
		selected:    selected,
		clock:       clock,
	}
}

func (v *ResourceView) Build() rty.Component {
	layout := rty.NewConcatLayout(rty.DirVert)
	layout.Add(v.resourceTitle())
	if v.res.IsCollapsed(v.rv) {
		return layout
	}

	layout.Add(v.resourceExpandedPane())
	return layout
}

func (v *ResourceView) resourceTitle() rty.Component {
	l := rty.NewConcatLayout(rty.DirHor)
	l.Add(v.titleTextName())
	l.Add(rty.TextString(" "))
	l.AddDynamic(rty.Fg(rty.NewFillerString('╌'), cLightText))
	l.Add(rty.TextString(" "))

	if tt := v.titleText(); tt != nil {
		l.Add(tt)
		l.Add(middotText())
	}

	l.Add(v.titleTextBuild())
	l.Add(middotText())
	l.Add(v.titleTextDeploy())
	return rty.OneLine(l)
}

type statusDisplay struct {
	color   tcell.Color
	spinner bool
}

// NOTE: This should be in-sync with combinedStatus in the web UI
func combinedStatus(res view.Resource) statusDisplay {
	currentBuild := res.CurrentBuild
	hasCurrentBuild := !currentBuild.Empty()
	hasPendingBuild := !res.PendingBuildSince.IsZero() && res.TriggerMode.AutoOnChange()
	buildHistory := res.BuildHistory
	lastBuild := res.LastBuild()
	lastBuildError := lastBuild.Error != nil

	if hasCurrentBuild {
		return statusDisplay{color: cPending, spinner: true}
	} else if hasPendingBuild {
		return statusDisplay{color: cPending}
	} else if lastBuildError {
		return statusDisplay{color: cBad}
	}

	runtimeStatus := model.RuntimeStatusUnknown
	if res.ResourceInfo != nil {
		runtimeStatus = res.ResourceInfo.RuntimeStatus()
	}

	switch runtimeStatus {
	case model.RuntimeStatusError:
		return statusDisplay{color: cBad}
	case model.RuntimeStatusPending:
		return statusDisplay{color: cPending, spinner: true}
	case model.RuntimeStatusOK:
		return statusDisplay{color: cGood}
	case model.RuntimeStatusNotApplicable:
		if len(buildHistory) > 0 {
			return statusDisplay{color: cGood}
		} else {
			return statusDisplay{color: cPending}
		}
	}
	return statusDisplay{color: cPending}
}

func (v *ResourceView) titleTextName() rty.Component {
	sb := rty.NewStringBuilder()
	selected := v.selected

	p := " "
	if selected {
		p = "▼"
		if runtime.GOOS == "windows" {
			// Windows default fonts support fewer symbols.
			p = "↓"
		}
	}
	if selected && v.res.IsCollapsed(v.rv) {
		p = "▶"
		if runtime.GOOS == "windows" {
			p = "→"
		}
	}

	display := combinedStatus(v.res)
	sb.Text(p)

	switch display.color {
	case cGood:
		sb.Fg(display.color).Textf(" ● ")
	case cBad:
		sb.Fg(display.color).Textf(" %s ", xMark())
	default:
		sb.Fg(display.color).Textf(" ○ ")
	}

	name := v.res.Name.String()
	if display.spinner {
		name = fmt.Sprintf("%s %s", v.res.Name, v.spinner())
	}
	if len(v.warnings()) > 0 {
		name = fmt.Sprintf("%s %s", v.res.Name, "— Warning ⚠️")
	}
	sb.Fg(tcell.ColorDefault).Text(name)
	return sb.Build()
}

func (v *ResourceView) warnings() []string {
	spanID := v.res.LastBuild().SpanID
	if spanID == "" {
		return nil
	}
	return v.logReader.Warnings(spanID)
}

func (v *ResourceView) titleText() rty.Component {
	switch i := v.res.ResourceInfo.(type) {
	case view.DCResourceInfo:
		return titleTextDC(i)
	case view.K8sResourceInfo:
		return titleTextK8s(i)
	default:
		return nil
	}
}

func titleTextK8s(k8sInfo view.K8sResourceInfo) rty.Component {
	status := k8sInfo.PodStatus
	if status == "" {
		status = "Pending"
	}
	return rty.TextString(status)
}

func titleTextDC(dcInfo view.DCResourceInfo) rty.Component {
	sb := rty.NewStringBuilder()
	status := dcInfo.Status()
	if status == "" {
		status = "Pending"
	}
	sb.Text(status)
	return sb.Build()
}

func (v *ResourceView) titleTextBuild() rty.Component {
	return buildStatusCell(makeBuildStatus(v.res, v.triggerMode))
}

func (v *ResourceView) titleTextDeploy() rty.Component {
	return deployTimeCell(v.res.LastDeployTime, tcell.ColorDefault)
}

func (v *ResourceView) resourceExpandedPane() rty.Component {
	l := rty.NewConcatLayout(rty.DirHor)
	l.Add(rty.TextString(strings.Repeat(" ", 4)))

	rhs := rty.NewConcatLayout(rty.DirVert)
	rhs.Add(v.resourceExpandedHistory())
	rhs.Add(v.resourceExpanded())
	rhs.Add(v.resourceExpandedEndpoints())
	rhs.Add(v.resourceExpandedError())
	l.AddDynamic(rhs)
	return l
}

func (v *ResourceView) resourceExpanded() rty.Component {
	switch v.res.ResourceInfo.(type) {
	case view.DCResourceInfo:
		return v.resourceExpandedDC()
	case view.K8sResourceInfo:
		return v.resourceExpandedK8s()
	case view.YAMLResourceInfo:
		return v.resourceExpandedYAML()
	default:
		return rty.EmptyLayout
	}
}

func (v *ResourceView) resourceExpandedYAML() rty.Component {
	yi := v.res.YAMLInfo()

	if !v.res.IsYAML() || len(yi.K8sDisplayNames) == 0 {
		return rty.EmptyLayout
	}

	l := rty.NewConcatLayout(rty.DirHor)
	l.Add(rty.TextString(strings.Repeat(" ", 2)))
	rhs := rty.NewConcatLayout(rty.DirVert)
	rhs.Add(rty.NewStringBuilder().Fg(cLightText).Text("(Kubernetes objects that don't match a group)").Build())
	rhs.Add(rty.TextString(strings.Join(yi.K8sDisplayNames, "\n")))
	l.AddDynamic(rhs)
	return l
}

func (v *ResourceView) resourceExpandedDC() rty.Component {
	dcInfo := v.res.DCInfo()

	l := rty.NewConcatLayout(rty.DirHor)
	l.Add(v.resourceTextDCContainer(dcInfo))
	l.Add(rty.TextString(" "))
	l.AddDynamic(rty.NewFillerString(' '))

	st := v.res.DockerComposeTarget().StartTime
	if !st.IsZero() {
		if len(v.res.Endpoints) > 0 {
			v.appendEndpoints(l)
			l.Add(middotText())
		}
		l.Add(resourceTextAge(st))
	}

	return rty.OneLine(l)
}

func (v *ResourceView) resourceTextDCContainer(dcInfo view.DCResourceInfo) rty.Component {
	if dcInfo.ContainerID.String() == "" {
		return rty.EmptyLayout
	}

	sb := rty.NewStringBuilder()
	sb.Fg(cLightText).Text("Container ID: ")
	sb.Fg(tcell.ColorDefault).Text(dcInfo.ContainerID.ShortStr())
	return sb.Build()
}

func (v *ResourceView) endpointsNeedSecondLine() bool {
	if len(v.res.Endpoints) > 1 {
		return true
	}
	if v.res.IsK8s() && v.res.K8sInfo().PodRestarts > 0 && len(v.res.Endpoints) == 1 {
		return true
	}
	return false
}

func (v *ResourceView) resourceExpandedK8s() rty.Component {
	k8sInfo := v.res.K8sInfo()
	if k8sInfo.PodName == "" {
		return rty.EmptyLayout
	}

	l := rty.NewConcatLayout(rty.DirHor)
	l.Add(resourceTextPodName(k8sInfo))
	l.Add(rty.TextString(" "))
	l.AddDynamic(rty.NewFillerString(' '))
	l.Add(rty.TextString(" "))

	if k8sInfo.PodRestarts > 0 {
		l.Add(resourceTextPodRestarts(k8sInfo))
		l.Add(middotText())
	}

	if len(v.res.Endpoints) > 0 && !v.endpointsNeedSecondLine() {
		v.appendEndpoints(l)
		l.Add(middotText())
	}

	l.Add(resourceTextAge(k8sInfo.PodCreationTime))
	return rty.OneLine(l)
}

func resourceTextPodName(k8sInfo view.K8sResourceInfo) rty.Component {
	sb := rty.NewStringBuilder()
	sb.Fg(cLightText).Text("K8S POD: ")
	sb.Fg(tcell.ColorDefault).Text(k8sInfo.PodName)
	return sb.Build()
}

func resourceTextPodRestarts(k8sInfo view.K8sResourceInfo) rty.Component {
	s := "restarts"
	if k8sInfo.PodRestarts == 1 {
		s = "restart"
	}
	return rty.NewStringBuilder().
		Fg(cPending).
		Textf("%d %s", k8sInfo.PodRestarts, s).
		Build()
}

func resourceTextAge(t time.Time) rty.Component {
	sb := rty.NewStringBuilder()
	sb.Fg(cLightText).Text("AGE ")
	sb.Fg(tcell.ColorDefault).Text(formatDeployAge(time.Since(t)))
	return rty.NewMinLengthLayout(DeployCellMinWidth, rty.DirHor).
		SetAlign(rty.AlignEnd).
		Add(sb.Build())
}

func (v *ResourceView) appendEndpoints(l *rty.ConcatLayout) {
	for i, endpoint := range v.res.Endpoints {
		if i != 0 {
			l.Add(middotText())
		}
		l.Add(rty.TextString(endpoint))
	}
}

func (v *ResourceView) resourceExpandedEndpoints() rty.Component {
	if !v.endpointsNeedSecondLine() {
		return rty.NewConcatLayout(rty.DirVert)
	}

	l := rty.NewConcatLayout(rty.DirHor)
	l.Add(resourceTextURLPrefix())
	v.appendEndpoints(l)

	return l
}

func resourceTextURLPrefix() rty.Component {
	sb := rty.NewStringBuilder()
	sb.Fg(cLightText).Text("URL: ")
	return sb.Build()
}

func (v *ResourceView) resourceExpandedHistory() rty.Component {
	if v.res.IsYAML() {
		return rty.NewConcatLayout(rty.DirVert)
	}

	if v.res.CurrentBuild.Empty() && len(v.res.BuildHistory) == 0 {
		return rty.NewConcatLayout(rty.DirVert)
	}

	l := rty.NewConcatLayout(rty.DirHor)
	l.Add(rty.NewStringBuilder().Fg(cLightText).Text("HISTORY: ").Build())

	rows := rty.NewConcatLayout(rty.DirVert)
	rowCount := 0
	if !v.res.CurrentBuild.Empty() {
		rows.Add(NewEditStatusLine(buildStatus{
			edits:    v.res.CurrentBuild.Edits,
			reason:   v.res.CurrentBuild.Reason,
			duration: v.res.CurrentBuild.Duration(),
			status:   "Building",
			muted:    true,
		}))
		rowCount++
	}
	for _, bStatus := range v.res.BuildHistory {
		if rowCount >= 2 {
			// at most 2 rows
			break
		}

		status := "OK"
		if bStatus.Error != nil {
			status = "Error"
		}

		rows.Add(NewEditStatusLine(buildStatus{
			edits:      bStatus.Edits,
			reason:     bStatus.Reason,
			duration:   bStatus.Duration(),
			status:     status,
			deployTime: bStatus.FinishTime,
		}))
		rowCount++
	}
	l.AddDynamic(rows)
	return l
}

func (v *ResourceView) resourceExpandedError() rty.Component {
	errPane, ok := v.resourceExpandedBuildError()
	isWarnings := false
	if !ok {
		errPane, ok = v.resourceExpandedRuntimeError()
	}
	if !ok {
		errPane, ok = v.resourceExpandedWarnings()
		if ok {
			isWarnings = true
		}
	}

	if !ok {
		return rty.NewConcatLayout(rty.DirVert)
	}

	l := rty.NewConcatLayout(rty.DirVert)
	if isWarnings {
		l.Add(rty.NewStringBuilder().Fg(cLightText).Text("WARNINGS:").Build())
	} else {
		l.Add(rty.NewStringBuilder().Fg(cLightText).Text("ERROR:").Build())
	}

	indentPane := rty.NewConcatLayout(rty.DirHor)
	indentPane.Add(rty.TextString(strings.Repeat(" ", 3)))

	errPane = rty.NewTailLayout(errPane)
	errPane = rty.NewMaxLengthLayout(errPane, rty.DirVert, MaxInlineErrHeight)
	indentPane.Add(errPane)
	l.Add(indentPane)

	return l
}

func (v *ResourceView) resourceExpandedRuntimeError() (rty.Component, bool) {
	pane := rty.NewConcatLayout(rty.DirVert)
	ok := false
	if isCrashing(v.res) {
		spanID := v.res.ResourceInfo.RuntimeSpanID()
		runtimeLog := v.logReader.TailSpan(abbreviatedLogLineCount, spanID)
		abbrevLog := abbreviateLog(runtimeLog)
		for _, logLine := range abbrevLog {
			pane.Add(rty.TextString(logLine))
			ok = true
		}
	}
	return pane, ok
}

func (v *ResourceView) resourceExpandedWarnings() (rty.Component, bool) {
	pane := rty.NewConcatLayout(rty.DirVert)
	ok := false

	warnings := v.warnings()
	if len(warnings) > 0 {
		abbrevLog := abbreviateLog(strings.Join(warnings, ""))
		for _, logLine := range abbrevLog {
			pane.Add(rty.TextString(logLine))
			ok = true
		}
	}
	return pane, ok
}

func (v *ResourceView) resourceExpandedBuildError() (rty.Component, bool) {
	pane := rty.NewConcatLayout(rty.DirVert)
	ok := false

	if v.res.LastBuild().Error != nil {
		spanID := v.res.LastBuild().SpanID
		abbrevLog := abbreviateLog(v.logReader.TailSpan(abbreviatedLogLineCount, spanID))
		for _, logLine := range abbrevLog {
			pane.Add(rty.TextString(logLine))
			ok = true
		}

		// if the build log is non-empty, it will contain the error, so we don't need to show this separately
		if len(abbrevLog) == 0 {
			pane.Add(rty.TextString(fmt.Sprintf("Error: %s", v.res.LastBuild().Error)))
			ok = true
		}
	}

	return pane, ok
}

var spinnerChars = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

var spinnerCharsWindows = []string{
	string(tview.BoxDrawingsLightDownAndRight),
	string(tview.BoxDrawingsLightHorizontal),
	string(tview.BoxDrawingsLightHorizontal),
	string(tview.BoxDrawingsLightDownAndLeft),
	string(tview.BoxDrawingsLightVertical),
	string(tview.BoxDrawingsLightUpAndLeft),
	string(tview.BoxDrawingsLightHorizontal),
	string(tview.BoxDrawingsLightHorizontal),
	string(tview.BoxDrawingsLightUpAndRight),
	string(tview.BoxDrawingsLightVertical),
}

func (v *ResourceView) spinner() string {
	chars := spinnerChars
	if runtime.GOOS == "windows" {
		chars = spinnerCharsWindows
	}
	decisecond := v.clock().Nanosecond() / int(time.Second/10)
	return chars[decisecond%len(chars)] // tick spinner every 10x/second
}
