package templates

import (
	"context"
	"encoding/json"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/common/model"

	"github.com/grafana/alerting/logging"
	"github.com/grafana/alerting/models"
)

type Template = template.Template
type KV = template.KV
type Data = template.Data

var FromGlobs = template.FromGlobs

type ExtendedAlert struct {
	Status        string             `json:"status"`
	Labels        KV                 `json:"labels"`
	Annotations   KV                 `json:"annotations"`
	StartsAt      time.Time          `json:"startsAt"`
	EndsAt        time.Time          `json:"endsAt"`
	GeneratorURL  string             `json:"generatorURL"`
	Fingerprint   string             `json:"fingerprint"`
	SilenceURL    string             `json:"silenceURL"`
	DashboardURL  string             `json:"dashboardURL"`
	PanelURL      string             `json:"panelURL"`
	Values        map[string]float64 `json:"values"`
	ValueString   string             `json:"valueString"` // TODO: Remove in Grafana 10
	ImageURL      string             `json:"imageURL,omitempty"`
	EmbeddedImage string             `json:"embeddedImage,omitempty"`
}

type ExtendedAlerts []ExtendedAlert

type ExtendedData struct {
	Receiver string         `json:"receiver"`
	Status   string         `json:"status"`
	Alerts   ExtendedAlerts `json:"alerts"`

	GroupLabels       KV `json:"groupLabels"`
	CommonLabels      KV `json:"commonLabels"`
	CommonAnnotations KV `json:"commonAnnotations"`

	ExternalURL string `json:"externalURL"`
}

func removePrivateItems(kv template.KV) template.KV {
	for key := range kv {
		if strings.HasPrefix(key, "__") && strings.HasSuffix(key, "__") {
			kv = kv.Remove([]string{key})
		}
	}
	return kv
}

func extendAlert(alert template.Alert, externalURL string, logger logging.Logger) *ExtendedAlert {
	// remove "private" annotations & labels so they don't show up in the template
	extended := &ExtendedAlert{
		Status:       alert.Status,
		Labels:       removePrivateItems(alert.Labels),
		Annotations:  removePrivateItems(alert.Annotations),
		StartsAt:     alert.StartsAt,
		EndsAt:       alert.EndsAt,
		GeneratorURL: alert.GeneratorURL,
		Fingerprint:  alert.Fingerprint,
	}

	// fill in some grafana-specific urls
	if len(externalURL) == 0 {
		return extended
	}
	u, err := url.Parse(externalURL)
	if err != nil {
		logger.Debug("failed to parse external URL while extending template data", "url", externalURL, "error", err.Error())
		return extended
	}
	externalPath := u.Path
	dashboardUID := alert.Annotations[models.DashboardUIDAnnotation]
	if len(dashboardUID) > 0 {
		u.Path = path.Join(externalPath, "/d/", dashboardUID)
		extended.DashboardURL = u.String()
		panelID := alert.Annotations[models.PanelIDAnnotation]
		if len(panelID) > 0 {
			u.RawQuery = "viewPanel=" + panelID
			extended.PanelURL = u.String()
		}

		generatorURL, err := url.Parse(extended.GeneratorURL)
		if err != nil {
			logger.Debug("failed to parse generator URL while extending template data", "url", extended.GeneratorURL, "err", err.Error())
			return extended
		}

		dashboardURL, err := url.Parse(extended.DashboardURL)
		if err != nil {
			logger.Debug("failed to parse dashboard URL while extending template data", "url", extended.DashboardURL, "err", err.Error())
			return extended
		}

		orgID := alert.Annotations[models.OrgIDAnnotation]
		if len(orgID) > 0 {
			extended.DashboardURL = setOrgIDQueryParam(dashboardURL, orgID)
			extended.PanelURL = setOrgIDQueryParam(u, orgID)
			extended.GeneratorURL = setOrgIDQueryParam(generatorURL, orgID)
		}
	}

	if alert.Annotations != nil {
		if s, ok := alert.Annotations[models.ValuesAnnotation]; ok {
			if err := json.Unmarshal([]byte(s), &extended.Values); err != nil {
				logger.Warn("failed to unmarshal values annotation", "error", err)
			}
		}

		// TODO: Remove in Grafana 10
		extended.ValueString = alert.Annotations[models.ValueStringAnnotation]
	}

	matchers := make([]string, 0)
	for key, value := range alert.Labels {
		if !(strings.HasPrefix(key, "__") && strings.HasSuffix(key, "__")) {
			matchers = append(matchers, key+"="+value)
		}
	}
	sort.Strings(matchers)
	u.Path = path.Join(externalPath, "/alerting/silence/new")

	query := make(url.Values)
	query.Add("alertmanager", "grafana")
	for _, matcher := range matchers {
		query.Add("matcher", matcher)
	}

	u.RawQuery = query.Encode()

	extended.SilenceURL = u.String()

	return extended
}

func setOrgIDQueryParam(url *url.URL, orgID string) string {
	q := url.Query()
	q.Set("orgId", orgID)
	url.RawQuery = q.Encode()

	return url.String()
}

func ExtendData(data *Data, logger logging.Logger) *ExtendedData {
	alerts := make([]ExtendedAlert, 0, len(data.Alerts))

	for _, alert := range data.Alerts {
		extendedAlert := extendAlert(alert, data.ExternalURL, logger)
		alerts = append(alerts, *extendedAlert)
	}

	extended := &ExtendedData{
		Receiver:          data.Receiver,
		Status:            data.Status,
		Alerts:            alerts,
		GroupLabels:       data.GroupLabels,
		CommonLabels:      removePrivateItems(data.CommonLabels),
		CommonAnnotations: removePrivateItems(data.CommonAnnotations),

		ExternalURL: data.ExternalURL,
	}
	return extended
}

func TmplText(ctx context.Context, tmpl *Template, alerts []*types.Alert, l logging.Logger, tmplErr *error) (func(string) string, *ExtendedData) {
	promTmplData := notify.GetTemplateData(ctx, tmpl, alerts, l)
	data := ExtendData(promTmplData, l)

	return func(name string) (s string) {
		if *tmplErr != nil {
			return
		}
		s, *tmplErr = tmpl.ExecuteTextString(name, data)
		return s
	}, data
}

// Firing returns the subset of alerts that are firing.
func (as ExtendedAlerts) Firing() []ExtendedAlert {
	res := []ExtendedAlert{}
	for _, a := range as {
		if a.Status == string(model.AlertFiring) {
			res = append(res, a)
		}
	}
	return res
}

// Resolved returns the subset of alerts that are resolved.
func (as ExtendedAlerts) Resolved() []ExtendedAlert {
	res := []ExtendedAlert{}
	for _, a := range as {
		if a.Status == string(model.AlertResolved) {
			res = append(res, a)
		}
	}
	return res
}
