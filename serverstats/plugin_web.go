package serverstats

import (
	"github.com/jonas747/discordgo"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/common/pubsub"
	"github.com/jonas747/yagpdb/serverstats/models"
	"github.com/jonas747/yagpdb/web"
	"github.com/karlseguin/rcache"
	"github.com/sirupsen/logrus"
	"github.com/volatiletech/null"
	"github.com/volatiletech/sqlboiler/boil"
	"goji.io"
	"goji.io/pat"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var WebStatsCache = rcache.New(cacheChartFetcher, time.Minute)

type FormData struct {
	Public         bool
	IgnoreChannels []int64 `valid:"channel,false"`
}

func (p *Plugin) InitWeb() {
	tmplPath := "templates/plugins/serverstats.html"
	if common.Testing {
		tmplPath = "../../serverstats/assets/serverstats.html"
	}
	web.Templates = template.Must(web.Templates.ParseFiles(tmplPath))

	statsCPMux := goji.SubMux()
	web.CPMux.Handle(pat.New("/stats"), statsCPMux)
	web.CPMux.Handle(pat.New("/stats/*"), statsCPMux)
	statsCPMux.Use(web.RequireGuildChannelsMiddleware)

	cpGetHandler := web.ControllerHandler(publicHandler(HandleStatsHtml, false), "cp_serverstats")
	statsCPMux.Handle(pat.Get(""), cpGetHandler)
	statsCPMux.Handle(pat.Get("/"), cpGetHandler)

	statsCPMux.Handle(pat.Post("/settings"), web.ControllerPostHandler(HandleSaveStatsSettings, cpGetHandler, FormData{}, "Updated serverstats settings"))
	statsCPMux.Handle(pat.Get("/daily_json"), web.APIHandler(publicHandlerJson(HandleStatsJson, false)))
	statsCPMux.Handle(pat.Get("/charts"), web.APIHandler(publicHandlerJson(HandleStatsCharts, false)))

	// Public
	web.ServerPublicMux.Handle(pat.Get("/stats"), web.RequireGuildChannelsMiddleware(web.ControllerHandler(publicHandler(HandleStatsHtml, true), "cp_serverstats")))
	web.ServerPublicMux.Handle(pat.Get("/stats/daily_json"), web.RequireGuildChannelsMiddleware(web.APIHandler(publicHandlerJson(HandleStatsJson, true))))
	web.ServerPublicMux.Handle(pat.Get("/stats/charts"), web.RequireGuildChannelsMiddleware(web.APIHandler(publicHandlerJson(HandleStatsCharts, true))))
}

type publicHandlerFunc func(w http.ResponseWriter, r *http.Request, publicAccess bool) (web.TemplateData, error)

func publicHandler(inner publicHandlerFunc, public bool) web.ControllerHandlerFunc {
	mw := func(w http.ResponseWriter, r *http.Request) (web.TemplateData, error) {
		return inner(w, r.WithContext(web.SetContextTemplateData(r.Context(), map[string]interface{}{"Public": public})), public)
	}

	return mw
}

// Somewhat dirty - should clean up this mess sometime
func HandleStatsHtml(w http.ResponseWriter, r *http.Request, isPublicAccess bool) (web.TemplateData, error) {
	activeGuild, templateData := web.GetBaseCPContextData(r.Context())

	config, err := GetConfig(r.Context(), activeGuild.ID)
	if err != nil {
		return templateData, common.ErrWithCaller(err)
	}

	templateData["Config"] = config
	templateData["ExtraHead"] = template.HTML(`
<link rel="stylesheet" href="/static/vendor/morris/morris.css" />
<link rel="stylesheet" href="/static/vendor/chartist/chartist.min.css" />
	`)

	return templateData, nil
}

func HandleSaveStatsSettings(w http.ResponseWriter, r *http.Request) (web.TemplateData, error) {
	ag, templateData := web.GetBaseCPContextData(r.Context())

	formData := r.Context().Value(common.ContextKeyParsedForm).(*FormData)

	stringedChannels := ""
	alreadyAdded := make([]int64, 0, len(formData.IgnoreChannels))
OUTER:
	for i, v := range formData.IgnoreChannels {
		// only add each once
		for _, ad := range alreadyAdded {
			if ad == v {
				continue OUTER
			}
		}

		// make sure the channel exists
		channelExists := false
		for _, ec := range ag.Channels {
			if ec.ID == v {
				channelExists = true
				break
			}
		}

		if !channelExists {
			continue
		}

		if i != 0 {
			stringedChannels += ","
		}

		alreadyAdded = append(alreadyAdded, v)
		stringedChannels += strconv.FormatInt(v, 10)
	}

	model := &models.ServerStatsConfig{
		GuildID:        ag.ID,
		Public:         null.BoolFrom(formData.Public),
		IgnoreChannels: null.StringFrom(stringedChannels),
		CreatedAt:      null.TimeFrom(time.Now()),
	}

	err := model.UpsertG(r.Context(), true, []string{"guild_id"}, boil.Whitelist("public", "ignore_channels"), boil.Infer())
	if err == nil {
		go pubsub.Publish("server_stats_invalidate_cache", ag.ID, nil)
	}

	return templateData, err
}

type publicHandlerFuncJson func(w http.ResponseWriter, r *http.Request, publicAccess bool) interface{}

func publicHandlerJson(inner publicHandlerFuncJson, public bool) web.CustomHandlerFunc {
	mw := func(w http.ResponseWriter, r *http.Request) interface{} {
		return inner(w, r.WithContext(web.SetContextTemplateData(r.Context(), map[string]interface{}{"Public": public})), public)
	}

	return mw
}

func HandleStatsJson(w http.ResponseWriter, r *http.Request, isPublicAccess bool) interface{} {
	activeGuild, _ := web.GetBaseCPContextData(r.Context())

	conf, err := GetConfig(r.Context(), activeGuild.ID)
	if err != nil {
		web.CtxLogger(r.Context()).WithError(err).Error("Failed retrieving stats config")
		w.WriteHeader(http.StatusInternalServerError)
		return nil
	}

	if !conf.Public && isPublicAccess {
		return nil
	}

	stats, err := RetrieveDailyStats(activeGuild.ID)
	if err != nil {
		web.CtxLogger(r.Context()).WithError(err).Error("Failed retrieving stats")
		w.WriteHeader(http.StatusInternalServerError)
		return nil
	}

	// Update the names to human readable ones, leave the ids in the name fields for the ones not available
	for _, cs := range stats.ChannelMessages {
		for _, channel := range activeGuild.Channels {
			if discordgo.StrID(channel.ID) == cs.Name {
				cs.Name = channel.Name
				break
			}
		}
	}

	return stats
}

type ChartResponse struct {
	Days        int                       `json:"days"`
	MemberData  []*MemberChartDataPeriod  `json:"member_chart_data"`
	MessageData []*MessageChartDataPeriod `json:"message_chart_data"`
}

func HandleStatsCharts(w http.ResponseWriter, r *http.Request, isPublicAccess bool) interface{} {
	activeGuild, _ := web.GetBaseCPContextData(r.Context())

	conf, err := GetConfig(r.Context(), activeGuild.ID)
	if err != nil {
		web.CtxLogger(r.Context()).WithError(err).Error("Failed retrieving stats config")
		w.WriteHeader(http.StatusInternalServerError)
		return nil
	}

	if !conf.Public && isPublicAccess {
		return nil
	}

	numDays := 7
	if r.URL.Query().Get("days") != "" {
		numDays, _ = strconv.Atoi(r.URL.Query().Get("days"))
		if numDays > 365 {
			numDays = 365
		}
	}

	stats := CacheGetCharts(activeGuild.ID, numDays)
	return stats
}

func CacheGetCharts(guildID int64, days int) *ChartResponse {
	actualDays := days
	if days < 7 {
		actualDays = 7
	}

	// default to full time stats
	if days != 30 && days != 365 && days > 7 {
		actualDays = -1
		days = -1
	}

	key := "charts:" + strconv.FormatInt(guildID, 10) + ":" + strconv.FormatInt(int64(days), 10)
	statsInterface := WebStatsCache.Get(key)
	if statsInterface == nil {
		return &ChartResponse{
			MemberData:  make([]*MemberChartDataPeriod, 0),
			MessageData: make([]*MessageChartDataPeriod, 0),
		}

	}

	stats := statsInterface.(*ChartResponse)
	cop := *stats
	if actualDays != days && days != -1 {
		cop.MemberData = cop.MemberData[:actualDays]
		cop.MessageData = cop.MessageData[:actualDays]
		cop.Days = actualDays
	}

	return statsInterface.(*ChartResponse)
}

func cacheChartFetcher(key string) interface{} {
	split := strings.Split(key, ":")
	if len(split) < 3 {
		logrus.Error("[serverstats] invalid cache key: ", key)
		return nil
	}

	guildID, _ := strconv.ParseInt(split[1], 10, 64)
	days, _ := strconv.Atoi(split[2])

	memberData, err := RetrieveMemberChartStats(guildID, days)
	if err != nil {
		logrus.WithError(err).WithField("cache_key", key).Error("failed retrieving member chart data")
		return nil
	}

	messageData, err := RetrieveMessageChartData(guildID, days)
	if err != nil {
		logrus.WithError(err).WithField("cache_key", key).Error("failed retrieving message chart data")
		return nil
	}

	return &ChartResponse{
		Days:        days,
		MemberData:  memberData,
		MessageData: messageData,
	}
}
