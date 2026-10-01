package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	geocodingURL = "https://geocoding-api.open-meteo.com/v1/search"
	forecastURL  = "https://api.open-meteo.com/v1/forecast"
	translateURL = "https://clients5.google.com/translate_a/t"
	wttrURL      = "https://wttr.in/"
)

var Metadata = &plugin.PluginMetadata{
	Name:        "weather",
	Description: "天气查询（Open-Meteo，wttr.in 备用）",
	DescEN:      "Weather lookup (Open-Meteo, wttr.in fallback)",
	Version:     "1.0.0",
	Author:      "PaperValet",
	MinVersion:  "0.1.0",
}

type WeatherPlugin struct {
	http *http.Client
}

func New() *WeatherPlugin {
	return &WeatherPlugin{http: &http.Client{Timeout: 10 * time.Second}}
}

func (p *WeatherPlugin) Name() string        { return "weather" }
func (p *WeatherPlugin) Description() string { return Metadata.Description }
func (p *WeatherPlugin) DescEN() string      { return Metadata.DescEN }

func (p *WeatherPlugin) Init(ctx context.Context, mgr plugin.Manager) error {
	return mgr.RegisterCommand(&plugin.Command{
		Name:        "weather",
		Description: "查询全球城市实时天气（Open-Meteo，中文城市名自动识别）",
		DescEN:      "Real-time weather for any city (Open-Meteo, Chinese names supported)",
		Usage:       "weather <城市名> | weather help",
		UsageEN:     "weather <city> | weather help",
		Plugin:      p.Name(),
		Category:    "tools",
		Handler:     p.handleWeather,
	})
}

func (p *WeatherPlugin) Start(ctx context.Context) error { return nil }
func (p *WeatherPlugin) Stop(ctx context.Context) error  { return nil }

// ---------------------------------------------------------------- data

type wmo struct{ icon, zh, en string }

var weatherCodes = map[int]wmo{
	0:  {"☀️", "晴朗", "Clear sky"},
	1:  {"🌤️", "大部晴朗", "Mainly clear"},
	2:  {"⛅", "部分多云", "Partly cloudy"},
	3:  {"☁️", "阴天", "Overcast"},
	45: {"🌫️", "有雾", "Fog"},
	48: {"🌫️", "沉积雾凇", "Depositing rime fog"},
	51: {"🌦️", "轻度细雨", "Light drizzle"},
	53: {"🌦️", "中度细雨", "Moderate drizzle"},
	55: {"🌦️", "密集细雨", "Dense drizzle"},
	56: {"🌨️", "轻度冻雨", "Light freezing drizzle"},
	57: {"🌨️", "密集冻雨", "Dense freezing drizzle"},
	61: {"🌧️", "轻度降雨", "Slight rain"},
	63: {"🌧️", "中度降雨", "Moderate rain"},
	65: {"🌧️", "强降雨", "Heavy rain"},
	66: {"🌨️", "轻度冻雨", "Light freezing rain"},
	67: {"🌨️", "强冻雨", "Heavy freezing rain"},
	71: {"❄️", "轻度降雪", "Slight snowfall"},
	73: {"❄️", "中度降雪", "Moderate snowfall"},
	75: {"❄️", "强降雪", "Heavy snowfall"},
	77: {"🌨️", "雪粒", "Snow grains"},
	80: {"🌦️", "轻度阵雨", "Slight rain showers"},
	81: {"🌧️", "中度阵雨", "Moderate rain showers"},
	82: {"⛈️", "强阵雨", "Violent rain showers"},
	85: {"🌨️", "轻度阵雪", "Slight snow showers"},
	86: {"🌨️", "强阵雪", "Heavy snow showers"},
	95: {"⛈️", "雷暴", "Thunderstorm"},
	96: {"⛈️", "轻度冰雹雷暴", "Thunderstorm with slight hail"},
	99: {"⛈️", "强冰雹雷暴", "Thunderstorm with heavy hail"},
}

var windDirsZh = []string{"北", "北东北", "东北", "东东北", "东", "东东南", "东南", "南东南",
	"南", "南西南", "西南", "西西南", "西", "西西北", "西北", "北西北"}
var windDirsEn = []string{"N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE",
	"S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"}

func windIndex(deg float64) int {
	ix := int(math.Round(math.Mod(math.Mod(deg, 360)+360, 360)/22.5)) % 16
	return ix
}

// quickMap mirrors the reference's fast path for common Chinese city names.
var quickMap = map[string]string{
	"北京": "Beijing", "上海": "Shanghai", "广州": "Guangzhou", "深圳": "Shenzhen",
	"成都": "Chengdu", "杭州": "Hangzhou", "武汉": "Wuhan", "西安": "Xi'an",
	"重庆": "Chongqing", "南京": "Nanjing", "天津": "Tianjin", "苏州": "Suzhou",
	"长沙": "Changsha", "郑州": "Zhengzhou", "青岛": "Qingdao", "大连": "Dalian",
	"厦门": "Xiamen", "香港": "Hong Kong", "澳门": "Macau", "台北": "Taipei",
	"东京": "Tokyo", "大阪": "Osaka", "京都": "Kyoto", "首尔": "Seoul",
	"釜山": "Busan", "曼谷": "Bangkok", "新加坡": "Singapore", "吉隆坡": "Kuala Lumpur",
	"雅加达": "Jakarta", "马尼拉": "Manila", "河内": "Hanoi", "胡志明市": "Ho Chi Minh City",
	"迪拜": "Dubai", "新德里": "New Delhi", "孟买": "Mumbai", "伦敦": "London",
	"巴黎": "Paris", "柏林": "Berlin", "罗马": "Rome", "马德里": "Madrid",
	"巴塞罗那": "Barcelona", "阿姆斯特丹": "Amsterdam", "莫斯科": "Moscow", "纽约": "New York",
	"洛杉矶": "Los Angeles", "旧金山": "San Francisco", "芝加哥": "Chicago", "华盛顿": "Washington",
	"波士顿": "Boston", "西雅图": "Seattle", "多伦多": "Toronto", "温哥华": "Vancouver",
	"悉尼": "Sydney", "墨尔本": "Melbourne", "奥克兰": "Auckland", "惠灵顿": "Wellington",
}

type geoResult struct {
	ID         int64   `json:"id"`
	Population int64   `json:"population"`
	Name       string  `json:"name"`
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
	Country    string  `json:"country"`
	Admin1     string  `json:"admin1"`
}

type forecast struct {
	Timezone string `json:"timezone"`
	Current  *struct {
		Time          string  `json:"time"`
		Temperature   float64 `json:"temperature_2m"`
		Humidity      float64 `json:"relative_humidity_2m"`
		Apparent      float64 `json:"apparent_temperature"`
		Precipitation float64 `json:"precipitation"`
		Rain          float64 `json:"rain"`
		Snowfall      float64 `json:"snowfall"`
		WeatherCode   int     `json:"weather_code"`
		CloudCover    float64 `json:"cloud_cover"`
		PressureMSL   float64 `json:"pressure_msl"`
		WindSpeed     float64 `json:"wind_speed_10m"`
		WindDirection float64 `json:"wind_direction_10m"`
		WindGusts     float64 `json:"wind_gusts_10m"`
	} `json:"current"`
	Daily *struct {
		TempMax []float64 `json:"temperature_2m_max"`
		TempMin []float64 `json:"temperature_2m_min"`
		Sunrise []string  `json:"sunrise"`
		Sunset  []string  `json:"sunset"`
		PrecSum []float64 `json:"precipitation_sum"`
		WindMax []float64 `json:"wind_speed_10m_max"`
	} `json:"daily"`
}

var errNotFound = errors.New("city not found")

// ---------------------------------------------------------------- handler

func (p *WeatherPlugin) help(ctx *plugin.CommandContext) string {
	return ctx.Tlocal(
		"🌤️ **天气查询**\n\n"+
			"**用法**\n"+
			plugin.Code("weather <城市名>")+" 查询指定城市实时天气\n\n"+
			"**示例**\n"+
			plugin.Code("weather 北京")+"\n"+
			plugin.Code("weather beijing")+"\n"+
			plugin.Code("weather New York")+"\n\n"+
			"**说明**\n"+
			"温度、体感、湿度、风速风向、气压、云量、降水、日出日落与天气提醒\n"+
			"中文城市名自动识别（必要时调用 Google 翻译）\n\n"+
			"💡 数据来源：Open-Meteo（免费、无需密钥），失败时回退 wttr.in",
		"🌤️ **Weather**\n\n"+
			"**Usage**\n"+
			plugin.Code("weather <city>")+" current weather for a city\n\n"+
			"**Examples**\n"+
			plugin.Code("weather London")+"\n"+
			plugin.Code("weather New York")+"\n"+
			plugin.Code("weather 东京")+"\n\n"+
			"**Details**\n"+
			"Temperature, feels-like, humidity, wind, pressure, clouds, precipitation, sunrise/sunset and warnings\n"+
			"Chinese city names are recognised automatically\n\n"+
			"💡 Data: Open-Meteo (free, keyless), falls back to wttr.in")
}

func (p *WeatherPlugin) handleWeather(ctx *plugin.CommandContext) error {
	if len(ctx.Args) == 0 {
		return ctx.Edit(p.help(ctx))
	}
	if a := strings.ToLower(ctx.Args[0]); len(ctx.Args) == 1 && (a == "help" || a == "h") {
		return ctx.Edit(p.help(ctx))
	}
	input := strings.TrimSpace(strings.Join(ctx.Args, " "))
	_ = ctx.Edit("🔍 " + ctx.Tlocal("正在识别城市…", "Resolving city…") + " " + plugin.Code(input))

	loc, err := p.resolve(ctx, input)
	if err == nil {
		name := locationName(loc)
		_ = ctx.Edit("🌡️ " + ctx.Tlocal("正在获取天气…", "Fetching weather…") + " " + plugin.Code(name))
		var fc *forecast
		fc, err = p.fetchForecast(ctx.Context(), loc)
		if err == nil {
			return ctx.Edit(buildReport(ctx, fc, name))
		}
	}
	if errors.Is(err, errNotFound) {
		return ctx.Edit(ctx.Tlocal(
			"❌ 城市未找到："+plugin.Code(input)+"\n\n💡 检查拼写、尝试英文名称或加上国家名，如 "+plugin.Code("weather Beijing China"),
			"❌ City not found: "+plugin.Code(input)+"\n\n💡 Check the spelling or add a country, e.g. "+plugin.Code("weather Paris France")))
	}

	// Open-Meteo failed (network / upstream). Try wttr.in as a fallback.
	if ctx.Logger != nil {
		ctx.Logger.Warn("weather: open-meteo failed, trying wttr.in", "err", err)
	}
	if out, werr := p.wttr(ctx, input); werr == nil {
		return ctx.Edit(out)
	}
	if isTimeout(err) {
		return ctx.Edit("❌ " + ctx.Tlocal("请求超时，网络连接缓慢，请稍后重试", "Request timed out, please retry later"))
	}
	return ctx.Edit("❌ " + ctx.Tlocal("查询失败：", "Lookup failed: ") + plugin.Escape(trim(err.Error(), 200)))
}

// resolve finds coordinates for user input: quick map → direct geocoding →
// Google-translated name (for Chinese input that the geocoder misses).
func (p *WeatherPlugin) resolve(ctx *plugin.CommandContext, input string) (*geoResult, error) {
	lang := "zh"
	if ctx.Lang == "en-US" {
		lang = "en"
	}
	candidates := []string{}
	if v, ok := quickMap[input]; ok {
		candidates = append(candidates, v)
	}
	candidates = append(candidates, input)

	var lastErr error = errNotFound
	tried := map[string]bool{}
	tryName := func(name string) (*geoResult, error) {
		if tried[strings.ToLower(name)] {
			return nil, errNotFound
		}
		tried[strings.ToLower(name)] = true
		return p.geocode(ctx.Context(), name, lang)
	}
	for _, c := range candidates {
		r, err := tryName(c)
		if err == nil {
			return r, nil
		}
		if !errors.Is(err, errNotFound) {
			lastErr = err
		}
	}
	if hasHan(input) {
		if en, err := p.translate(ctx.Context(), input); err == nil && en != "" {
			_ = ctx.Edit("🌍 " + ctx.Tlocal("正在搜索…", "Searching…") + " " + plugin.Code(input+" → "+en))
			r, err := tryName(en)
			if err == nil {
				return r, nil
			}
			if !errors.Is(err, errNotFound) {
				lastErr = err
			}
		}
	}
	return nil, lastErr
}

func (p *WeatherPlugin) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "PaperValet-Weather/1.1")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func (p *WeatherPlugin) geocodeRaw(ctx context.Context, name, lang string) ([]geoResult, error) {
	q := url.Values{"name": {name}, "count": {"10"}, "language": {lang}, "format": {"json"}}
	var data struct {
		Results []geoResult `json:"results"`
	}
	if err := p.getJSON(ctx, geocodingURL+"?"+q.Encode(), &data); err != nil {
		return nil, err
	}
	return data.Results, nil
}

// geocode returns the best match for name. Like the reference it searches
// the localized (zh) index, but that index sometimes misses the obvious city
// (e.g. "New York" → York, Nebraska), so for Latin input the English index is
// consulted too and exact name matches win, largest population first.
func (p *WeatherPlugin) geocode(ctx context.Context, name, lang string) (*geoResult, error) {
	res, err := p.geocodeRaw(ctx, name, lang)
	if err != nil {
		return nil, err
	}
	all := res
	if lang != "en" && !hasHan(name) {
		if en, err := p.geocodeRaw(ctx, name, "en"); err == nil {
			all = append(append([]geoResult{}, res...), en...)
		}
	}
	best := exactMatch(all, name)
	if best == nil {
		if len(all) == 0 {
			return nil, errNotFound
		}
		best = &all[0]
	}
	for i := range res { // prefer the localized entry of the same place
		if res[i].ID == best.ID {
			return &res[i], nil
		}
	}
	return best, nil
}

// exactMatch picks the most populous result whose name equals name.
func exactMatch(res []geoResult, name string) *geoResult {
	var best *geoResult
	for i := range res {
		if !strings.EqualFold(strings.TrimSpace(res[i].Name), strings.TrimSpace(name)) {
			continue
		}
		if best == nil || res[i].Population > best.Population {
			best = &res[i]
		}
	}
	return best
}

func (p *WeatherPlugin) fetchForecast(ctx context.Context, loc *geoResult) (*forecast, error) {
	q := url.Values{
		"latitude":      {strconv.FormatFloat(loc.Latitude, 'f', -1, 64)},
		"longitude":     {strconv.FormatFloat(loc.Longitude, 'f', -1, 64)},
		"current":       {"temperature_2m,relative_humidity_2m,apparent_temperature,precipitation,rain,snowfall,weather_code,cloud_cover,pressure_msl,wind_speed_10m,wind_direction_10m,wind_gusts_10m"},
		"daily":         {"weather_code,temperature_2m_max,temperature_2m_min,sunrise,sunset,precipitation_sum,wind_speed_10m_max"},
		"timezone":      {"auto"},
		"forecast_days": {"1"},
	}
	var fc forecast
	if err := p.getJSON(ctx, forecastURL+"?"+q.Encode(), &fc); err != nil {
		return nil, err
	}
	if fc.Current == nil {
		return nil, errors.New("no current weather data")
	}
	return &fc, nil
}

// translate converts a (Chinese) place name to English via Google's keyless
// dictionary endpoint. Returns the input's translation or an error.
func (p *WeatherPlugin) translate(ctx context.Context, text string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	q := url.Values{"client": {"dict-chrome-ex"}, "sl": {"auto"}, "tl": {"en"}, "q": {text}}
	var raw []any
	if err := p.getJSON(c, translateURL+"?"+q.Encode(), &raw); err != nil {
		return "", err
	}
	// [["translation","src"]] or ["translation"]
	if len(raw) > 0 {
		switch v := raw[0].(type) {
		case string:
			return strings.TrimSpace(v), nil
		case []any:
			if len(v) > 0 {
				if s, ok := v[0].(string); ok {
					return strings.TrimSpace(s), nil
				}
			}
		}
	}
	return "", errors.New("empty translation")
}

// ---------------------------------------------------------------- output

func locationName(l *geoResult) string {
	parts := []string{}
	if l.Name != "" && l.Name != "undefined" {
		parts = append(parts, l.Name)
	}
	if l.Admin1 != "" && l.Admin1 != "undefined" && l.Admin1 != l.Name {
		parts = append(parts, l.Admin1)
	}
	if l.Country != "" && l.Country != "undefined" {
		parts = append(parts, l.Country)
	}
	return strings.Join(parts, ", ")
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func hhmm(s string) string {
	if i := strings.Index(s, "T"); i >= 0 && len(s) >= i+6 {
		return s[i+1 : i+6]
	}
	return s
}

func field(label, value string) string { return label + "  " + plugin.Code(value) + "\n" }

func buildReport(ctx *plugin.CommandContext, fc *forecast, name string) string {
	cur := fc.Current
	info, ok := weatherCodes[cur.WeatherCode]
	if !ok {
		info = wmo{"🌤️", "未知", "Unknown"}
	}
	en := ctx.Lang == "en-US"
	desc := info.zh
	dir := windDirsZh[windIndex(cur.WindDirection)] + "风"
	if en {
		desc = info.en
		dir = windDirsEn[windIndex(cur.WindDirection)]
	}

	var b strings.Builder
	b.WriteString(info.icon + " " + plugin.Bold(name) + "\n\n")
	b.WriteString("**" + ctx.Tlocal("当前天气", "Current") + "**\n")
	b.WriteString(field(ctx.Tlocal("天气", "Condition"), desc))
	b.WriteString(field(ctx.Tlocal("温度", "Temperature"), num(cur.Temperature)+"°C"))
	b.WriteString(field(ctx.Tlocal("体感", "Feels like"), num(cur.Apparent)+"°C"))
	if d := fc.Daily; d != nil && len(d.TempMax) > 0 && len(d.TempMin) > 0 {
		b.WriteString(field(ctx.Tlocal("今日最高/最低", "Today high/low"), num(d.TempMax[0])+"°C / "+num(d.TempMin[0])+"°C"))
	}
	b.WriteString(field(ctx.Tlocal("湿度", "Humidity"), num(cur.Humidity)+"%"))
	b.WriteString(field(ctx.Tlocal("风速", "Wind"), num(cur.WindSpeed)+" km/h ("+dir+")"))
	if cur.WindGusts > 0 {
		b.WriteString(field(ctx.Tlocal("阵风", "Gusts"), num(cur.WindGusts)+" km/h"))
	}
	b.WriteString(field(ctx.Tlocal("气压", "Pressure"), strconv.Itoa(int(math.Round(cur.PressureMSL)))+" hPa"))
	b.WriteString(field(ctx.Tlocal("云量", "Cloud cover"), num(cur.CloudCover)+"%"))
	if cur.Precipitation > 0 {
		b.WriteString(field(ctx.Tlocal("降水量", "Precipitation"), num(cur.Precipitation)+" mm"))
	}
	if cur.Rain > 0 {
		b.WriteString(field(ctx.Tlocal("降雨量", "Rain"), num(cur.Rain)+" mm"))
	}
	if cur.Snowfall > 0 {
		b.WriteString(field(ctx.Tlocal("降雪量", "Snowfall"), num(cur.Snowfall)+" cm"))
	}
	if d := fc.Daily; d != nil && len(d.Sunrise) > 0 && len(d.Sunset) > 0 {
		b.WriteString(field(ctx.Tlocal("日出", "Sunrise"), hhmm(d.Sunrise[0])))
		b.WriteString(field(ctx.Tlocal("日落", "Sunset"), hhmm(d.Sunset[0])))
	}

	if w := warnings(ctx, cur.Temperature, cur.WindSpeed, cur.Precipitation, cur.WeatherCode); len(w) > 0 {
		b.WriteString("\n**" + ctx.Tlocal("⚠️ 天气提醒", "⚠️ Warnings") + "**\n")
		for _, s := range w {
			b.WriteString(plugin.Escape(s) + "\n")
		}
	}
	b.WriteString("\n💡 " + ctx.Tlocal("数据来源：Open-Meteo", "Data: Open-Meteo"))
	if fc.Timezone != "" {
		b.WriteString(" · " + plugin.Escape(fc.Timezone))
	}
	return b.String()
}

func warnings(ctx *plugin.CommandContext, temp, wind, prec float64, code int) []string {
	var w []string
	if temp > 35 {
		w = append(w, ctx.Tlocal("🔥 高温预警：", "🔥 Heat warning: ")+num(temp)+"°C")
	} else if temp < -10 {
		w = append(w, ctx.Tlocal("❄️ 低温预警：", "❄️ Cold warning: ")+num(temp)+"°C")
	}
	if wind > 40 {
		w = append(w, ctx.Tlocal("💨 大风预警：风速 ", "💨 Wind warning: ")+num(wind)+" km/h")
	}
	if prec > 10 {
		w = append(w, ctx.Tlocal("🌧️ 强降水预警：", "🌧️ Heavy precipitation: ")+num(prec)+" mm")
	}
	switch {
	case code >= 95 && code <= 99:
		w = append(w, ctx.Tlocal("⛈️ 雷暴预警：请注意安全", "⛈️ Thunderstorm: stay safe"))
	case code >= 71 && code <= 77:
		w = append(w, ctx.Tlocal("🌨️ 降雪预警：路面可能结冰", "🌨️ Snow: roads may be icy"))
	case code == 45 || code == 48:
		w = append(w, ctx.Tlocal("🌫️ 大雾预警：能见度低", "🌫️ Fog: low visibility"))
	}
	return w
}

// ---------------------------------------------------------------- wttr.in fallback

type wttrResp struct {
	CurrentCondition []struct {
		TempC      string `json:"temp_C"`
		FeelsLikeC string `json:"FeelsLikeC"`
		Humidity   string `json:"humidity"`
		WindKmph   string `json:"windspeedKmph"`
		WindDir    string `json:"winddir16Point"`
		Pressure   string `json:"pressure"`
		Cloudcover string `json:"cloudcover"`
		PrecipMM   string `json:"precipMM"`
		LangZh     []struct {
			Value string `json:"value"`
		} `json:"lang_zh"`
		WeatherDesc []struct {
			Value string `json:"value"`
		} `json:"weatherDesc"`
	} `json:"current_condition"`
	NearestArea []struct {
		AreaName []struct {
			Value string `json:"value"`
		} `json:"areaName"`
		Country []struct {
			Value string `json:"value"`
		} `json:"country"`
	} `json:"nearest_area"`
	Weather []struct {
		MaxTempC  string `json:"maxtempC"`
		MinTempC  string `json:"mintempC"`
		Astronomy []struct {
			Sunrise string `json:"sunrise"`
			Sunset  string `json:"sunset"`
		} `json:"astronomy"`
	} `json:"weather"`
}

func (p *WeatherPlugin) wttr(ctx *plugin.CommandContext, city string) (string, error) {
	lang := "zh"
	if ctx.Lang == "en-US" {
		lang = "en"
	}
	var data wttrResp
	if err := p.getJSON(ctx.Context(), wttrURL+url.PathEscape(city)+"?format=j1&lang="+lang, &data); err != nil {
		return "", err
	}
	if len(data.CurrentCondition) == 0 {
		return "", errNotFound
	}
	cur := data.CurrentCondition[0]
	desc := ""
	if lang == "zh" && len(cur.LangZh) > 0 {
		desc = cur.LangZh[0].Value
	} else if len(cur.WeatherDesc) > 0 {
		desc = cur.WeatherDesc[0].Value
	}
	area := city
	if len(data.NearestArea) > 0 && len(data.NearestArea[0].AreaName) > 0 {
		area = data.NearestArea[0].AreaName[0].Value
		if len(data.NearestArea[0].Country) > 0 {
			area += ", " + data.NearestArea[0].Country[0].Value
		}
	}
	var b strings.Builder
	b.WriteString("🌤️ " + plugin.Bold(area) + "\n\n")
	b.WriteString("**" + ctx.Tlocal("当前天气", "Current") + "**\n")
	if desc != "" {
		b.WriteString(field(ctx.Tlocal("天气", "Condition"), desc))
	}
	b.WriteString(field(ctx.Tlocal("温度", "Temperature"), cur.TempC+"°C"))
	b.WriteString(field(ctx.Tlocal("体感", "Feels like"), cur.FeelsLikeC+"°C"))
	if len(data.Weather) > 0 {
		b.WriteString(field(ctx.Tlocal("今日最高/最低", "Today high/low"), data.Weather[0].MaxTempC+"°C / "+data.Weather[0].MinTempC+"°C"))
	}
	b.WriteString(field(ctx.Tlocal("湿度", "Humidity"), cur.Humidity+"%"))
	b.WriteString(field(ctx.Tlocal("风速", "Wind"), cur.WindKmph+" km/h ("+cur.WindDir+")"))
	if cur.Pressure != "" {
		b.WriteString(field(ctx.Tlocal("气压", "Pressure"), cur.Pressure+" hPa"))
	}
	if cur.Cloudcover != "" {
		b.WriteString(field(ctx.Tlocal("云量", "Cloud cover"), cur.Cloudcover+"%"))
	}
	if v, err := strconv.ParseFloat(cur.PrecipMM, 64); err == nil && v > 0 {
		b.WriteString(field(ctx.Tlocal("降水量", "Precipitation"), cur.PrecipMM+" mm"))
	}
	if len(data.Weather) > 0 && len(data.Weather[0].Astronomy) > 0 {
		a := data.Weather[0].Astronomy[0]
		b.WriteString(field(ctx.Tlocal("日出", "Sunrise"), a.Sunrise))
		b.WriteString(field(ctx.Tlocal("日落", "Sunset"), a.Sunset))
	}
	b.WriteString("\n💡 " + ctx.Tlocal("数据来源：wttr.in（Open-Meteo 不可用，已回退）", "Data: wttr.in (Open-Meteo unavailable)"))
	return b.String(), nil
}

// ---------------------------------------------------------------- helpers

func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout")
}

func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
