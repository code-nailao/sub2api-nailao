package portal

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

//go:embed home-default.json
var defaultHome []byte

type HomeContent struct {
	SiteName    string    `json:"site_name"`
	LogoLight   string    `json:"logo_light"`
	LogoDark    string    `json:"logo_dark"`
	DocURL      string    `json:"doc_url"`
	WelfarePath string    `json:"welfare_path"`
	Brand       HomeBrand `json:"brand"`
}
type HomeLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}
type HomeBrand struct {
	Sections struct {
		Contacts bool `json:"contacts"`
		Pricing  bool `json:"pricing"`
		Features bool `json:"features"`
		Stats    bool `json:"stats"`
	} `json:"sections"`
	HeroBadge       string `json:"hero_badge"`
	HeroTitle       string `json:"hero_title"`
	HeroSubtitle    string `json:"hero_subtitle"`
	PrimaryLabel    string `json:"primary_cta_label"`
	PrimaryURL      string `json:"primary_cta_url"`
	SecondaryLabel  string `json:"secondary_cta_label"`
	SecondaryURL    string `json:"secondary_cta_url"`
	ExperienceTitle string `json:"experience_title"`
	Announcement    struct {
		Enabled bool   `json:"enabled"`
		Text    string `json:"text"`
	} `json:"announcement"`
	Highlights []struct {
		Label string `json:"label"`
	} `json:"highlights"`
	Contacts []struct {
		Name    string `json:"name"`
		Account string `json:"account"`
		Logo    string `json:"logo"`
		QRImage string `json:"qr_image"`
		URL     string `json:"url"`
	} `json:"contacts"`
	Features []struct {
		Icon        string `json:"icon"`
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"features"`
	Stats []struct {
		Value string `json:"value"`
		Label string `json:"label"`
	} `json:"stats"`
	FooterGroups []struct {
		Title string     `json:"title"`
		Links []HomeLink `json:"links"`
	} `json:"footer_groups"`
	PricingTable struct {
		Headings []string `json:"headings"`
		Rows     []struct {
			Model      string `json:"model"`
			Spec       string `json:"spec,omitempty"`
			Group      string `json:"group"`
			Multiplier string `json:"multiplier"`
			Input      string `json:"input"`
			Output     string `json:"output"`
			Official   string `json:"official"`
			Discount   string `json:"discount"`
		} `json:"rows"`
	} `json:"pricing_table"`
}
type HomeVersion struct {
	ID          int64       `json:"id"`
	PublishedAt time.Time   `json:"published_at"`
	PublishedBy int64       `json:"published_by"`
	Config      HomeContent `json:"config"`
}
type HomeState struct {
	Revision int64         `json:"revision"`
	Draft    HomeContent   `json:"draft"`
	Versions []HomeVersion `json:"versions"`
}
type ContentStore struct {
	mu     sync.Mutex
	path   string
	state  HomeState
	failed error
	writer func(string, any) error
}

var welfarePath = regexp.MustCompile(`^/custom/[A-Za-z0-9_-]{1,80}$`)

func validateContent(c HomeContent) error {
	if len(c.SiteName) < 1 || len(c.SiteName) > 120 || !welfarePath.MatchString(c.WelfarePath) || c.Brand.HeroTitle == "" {
		return errors.New("invalid_home_content")
	}
	if c.Brand.Contacts == nil || c.Brand.Highlights == nil || c.Brand.Features == nil || c.Brand.Stats == nil || c.Brand.FooterGroups == nil || c.Brand.PricingTable.Rows == nil || c.Brand.PricingTable.Headings == nil {
		return errors.New("home_arrays_required")
	}
	b, _ := json.Marshal(c)
	if len(b) > 30000 {
		return errors.New("home_content_too_large")
	}
	var obj any
	_ = json.Unmarshal(b, &obj)
	var walk func(any, string) bool
	walk = func(v any, key string) bool {
		switch x := v.(type) {
		case string:
			if len(x) > 4096 {
				return false
			}
			if key == "url" || key == "logo" || key == "qr_image" || key == "logo_light" || key == "logo_dark" || key == "doc_url" || key == "primary_cta_url" || key == "secondary_cta_url" {
				if x == "" {
					return true
				}
				u, e := url.Parse(x)
				if e != nil || u.User != nil || u.Opaque != "" {
					return false
				}
				if u.Scheme == "https" && u.Host != "" {
					return true
				}
				return u.Scheme == "" && u.Host == "" && (x[0] == '/' && len(x) > 1 && x[1] != '/' || x[0] == '#') && !bytes.ContainsAny([]byte(x), "\\\r\n\t")
			}
		case []any:
			if len(x) > 32 {
				return false
			}
			for _, v := range x {
				if !walk(v, key) {
					return false
				}
			}
		case map[string]any:
			for k, v := range x {
				if !walk(v, k) {
					return false
				}
			}
		}
		return true
	}
	if !walk(obj, "") {
		return errors.New("invalid_home_link_or_size")
	}
	for _, f := range c.Brand.Features {
		if f.Icon != "zap" && f.Icon != "route" && f.Icon != "pie-chart" && f.Icon != "blocks" {
			return errors.New("invalid_home_icon")
		}
	}
	return nil
}
func OpenContentStore(dir string) (*ContentStore, error) {
	s := &ContentStore{path: filepath.Join(dir, "home.json"), writer: writeJSONAtomic}
	b, e := os.ReadFile(s.path)
	if os.IsNotExist(e) {
		var c HomeContent
		d := json.NewDecoder(bytes.NewReader(defaultHome))
		d.DisallowUnknownFields()
		if e = d.Decode(&c); e != nil {
			return nil, e
		}
		if e = validateContent(c); e != nil {
			return nil, e
		}
		s.state = HomeState{Revision: 1, Draft: c, Versions: []HomeVersion{{ID: 1, Config: c}}}
		return s, nil
	}
	if e != nil {
		return nil, e
	}
	if json.Unmarshal(b, &s.state) != nil || s.state.Revision < 1 || len(s.state.Versions) < 1 {
		return nil, errors.New("invalid_home_store")
	}
	if e = validateContent(s.state.Draft); e != nil {
		return nil, e
	}
	for _, v := range s.state.Versions {
		if e = validateContent(v.Config); e != nil {
			return nil, e
		}
	}
	return s, nil
}
func (s *ContentStore) Read() HomeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.state)
	var state HomeState
	_ = json.Unmarshal(b, &state)
	return state
}
func (s *ContentStore) Change(action string, revision, version, admin int64, c *HomeContent) (HomeState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return HomeState{}, ErrPersistenceUncertain
	}
	if revision != s.state.Revision {
		return HomeState{}, ErrConflict
	}
	b, _ := json.Marshal(s.state)
	var next HomeState
	_ = json.Unmarshal(b, &next)
	switch action {
	case "draft":
		if c == nil {
			return HomeState{}, errors.New("invalid_home_content")
		}
		if e := validateContent(*c); e != nil {
			return HomeState{}, e
		}
		next.Draft = *c
	case "publish":
		next.Versions = append(next.Versions, HomeVersion{ID: next.Revision + 1, PublishedAt: time.Now().UTC(), PublishedBy: admin, Config: next.Draft})
	case "restore":
		found := false
		for _, v := range next.Versions {
			if v.ID == version {
				next.Draft = v.Config
				found = true
				break
			}
		}
		if !found {
			return HomeState{}, ErrNotFound
		}
	default:
		return HomeState{}, ErrNotFound
	}
	next.Revision++

	e := s.writer(s.path, next)
	if e == nil || writeCommitted(e) {
		s.state = next
	}
	if e != nil {
		s.failed = e
		return HomeState{}, e
	}
	return next, nil
}

// Public reads copy one published document, never the entire editing history.
func (s *ContentStore) Published() HomeContent {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.state.Versions[len(s.state.Versions)-1].Config)
	var result HomeContent
	_ = json.Unmarshal(b, &result)
	return result
}
