// Copyright 2019 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hugolib

import (
	"context"
	"fmt"
	"io"
	"log"
	"mime"
	"net/url"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gohugoio/hugo/common/herrors"
	"github.com/gohugoio/hugo/common/htime"
	"github.com/gohugoio/hugo/common/hugio"
	"github.com/gohugoio/hugo/common/types"
	"golang.org/x/text/unicode/norm"

	"github.com/gohugoio/hugo/common/paths"

	"github.com/gohugoio/hugo/identity"

	"github.com/gohugoio/hugo/markup/converter/hooks"

	"github.com/gohugoio/hugo/markup/converter"

	"github.com/gohugoio/hugo/hugofs/files"
	hglob "github.com/gohugoio/hugo/hugofs/glob"

	"github.com/gohugoio/hugo/common/maps"

	"github.com/gohugoio/hugo/common/text"

	"github.com/gohugoio/hugo/publisher"

	"github.com/gohugoio/hugo/langs"

	"github.com/gohugoio/hugo/resources/page"

	"github.com/gohugoio/hugo/config"
	"github.com/gohugoio/hugo/lazy"

	"github.com/fsnotify/fsnotify"
	bp "github.com/gohugoio/hugo/bufferpool"
	"github.com/gohugoio/hugo/deps"
	"github.com/gohugoio/hugo/helpers"
	"github.com/gohugoio/hugo/navigation"
	"github.com/gohugoio/hugo/output"
	"github.com/gohugoio/hugo/source"
	"github.com/gohugoio/hugo/tpl"

	"github.com/spf13/afero"
)

func (s *Site) Taxonomies() page.TaxonomyList {
	s.init.taxonomies.Do(context.Background())
	return s.taxonomies
}

type taxonomiesConfig map[string]string

func (t taxonomiesConfig) Values() []viewName {
	var vals []viewName
	for k, v := range t {
		vals = append(vals, viewName{singular: k, plural: v})
	}
	sort.Slice(vals, func(i, j int) bool {
		return vals[i].plural < vals[j].plural
	})

	return vals
}

type siteConfigHolder struct {
	sitemap          config.SitemapConfig
	taxonomiesConfig taxonomiesConfig
	timeout          time.Duration
	hasCJKLanguage   bool
	enableEmoji      bool
}

// Lazily loaded site dependencies.
type siteInit struct {
	prevNext          *lazy.Init
	prevNextInSection *lazy.Init
	menus             *lazy.Init
	taxonomies        *lazy.Init
}

func (init *siteInit) Reset() {
	init.prevNext.Reset()
	init.prevNextInSection.Reset()
	init.menus.Reset()
	init.taxonomies.Reset()
}

func (s *Site) initInit(ctx context.Context, init *lazy.Init, pctx pageContext) bool {
	_, err := init.Do(ctx)

	if err != nil {
		s.h.FatalError(pctx.wrapError(err))
	}
	return err == nil
}

func (s *Site) prepareInits() {
	s.init = &siteInit{}

	var init lazy.Init

	s.init.prevNext = init.Branch(func(context.Context) (any, error) {
		regularPages := s.RegularPages()
		for i, p := range regularPages {
			np, ok := p.(nextPrevProvider)
			if !ok {
				continue
			}

			pos := np.getNextPrev()
			if pos == nil {
				continue
			}

			pos.nextPage = nil
			pos.prevPage = nil

			if i > 0 {
				pos.nextPage = regularPages[i-1]
			}

			if i < len(regularPages)-1 {
				pos.prevPage = regularPages[i+1]
			}
		}
		return nil, nil
	})

	s.init.prevNextInSection = init.Branch(func(context.Context) (any, error) {
		var sections page.Pages
		s.home.treeRef.m.collectSectionsRecursiveIncludingSelf(pageMapQuery{Prefix: s.home.treeRef.key}, func(n *contentNode) {
			sections = append(sections, n.p)
		})

		setNextPrev := func(pas page.Pages) {
			for i, p := range pas {
				np, ok := p.(nextPrevInSectionProvider)
				if !ok {
					continue
				}

				pos := np.getNextPrevInSection()
				if pos == nil {
					continue
				}

				pos.nextPage = nil
				pos.prevPage = nil

				if i > 0 {
					pos.nextPage = pas[i-1]
				}

				if i < len(pas)-1 {
					pos.prevPage = pas[i+1]
				}
			}
		}

		for _, sect := range sections {
			treeRef := sect.(treeRefProvider).getTreeRef()

			var pas page.Pages
			treeRef.m.collectPages(pageMapQuery{Prefix: treeRef.key + cmBranchSeparator}, func(c *contentNode) {
				pas = append(pas, c.p)
			})
			page.SortByDefault(pas)

			setNextPrev(pas)
		}

		// The root section only goes one level down.
		treeRef := s.home.getTreeRef()

		var pas page.Pages
		treeRef.m.collectPages(pageMapQuery{Prefix: treeRef.key + cmBranchSeparator}, func(c *contentNode) {
			pas = append(pas, c.p)
		})
		page.SortByDefault(pas)

		setNextPrev(pas)

		return nil, nil
	})

	s.init.menus = init.Branch(func(context.Context) (any, error) {
		s.assembleMenus()
		return nil, nil
	})

	s.init.taxonomies = init.Branch(func(context.Context) (any, error) {
		err := s.pageMap.assembleTaxonomies()
		return nil, err
	})
}

type siteRenderingContext struct {
	output.Format
}

func (s *Site) Menus() navigation.Menus {
	s.init.menus.Do(context.Background())
	return s.menus
}

func (s *Site) initRenderFormats() {
	formatSet := make(map[string]bool)
	formats := output.Formats{}
	rssDisabled := !s.conf.IsKindEnabled("rss")
	s.pageMap.pageTrees.WalkRenderable(func(s string, n *contentNode) bool {
		for _, f := range n.p.m.configuredOutputFormats {
			if rssDisabled && f.Name == "rss" {
				// legacy
				continue
			}
			if !formatSet[f.Name] {
				formats = append(formats, f)
				formatSet[f.Name] = true
			}
		}
		return false
	})

	// Add the per kind configured output formats
	for _, kind := range allKindsInPages {
		if siteFormats, found := s.conf.C.KindOutputFormats[kind]; found {
			for _, f := range siteFormats {
				if !formatSet[f.Name] {
					formats = append(formats, f)
					formatSet[f.Name] = true
				}
			}
		}
	}

	sort.Sort(formats)
	s.renderFormats = formats
}

func (s *Site) GetRelatedDocsHandler() *page.RelatedDocsHandler {
	return s.relatedDocsHandler
}

func (s *Site) Language() *langs.Language {
	return s.language
}

func (s *Site) Languages() langs.Languages {
	return s.h.Configs.Languages
}

func (s *Site) isEnabled(kind string) bool {
	if kind == kindUnknown {
		panic("Unknown kind")
	}
	return !s.disabledKinds[kind]
}

// reset returns a new Site prepared for rebuild.
func (s *Site) reset() *Site {
	return &Site{
		Deps:                s.Deps,
		disabledKinds:       s.disabledKinds,
		titleFunc:           s.titleFunc,
		relatedDocsHandler:  s.relatedDocsHandler.Clone(),
		siteRefLinker:       s.siteRefLinker,
		outputFormats:       s.outputFormats,
		rc:                  s.rc,
		outputFormatsConfig: s.outputFormatsConfig,
		frontmatterHandler:  s.frontmatterHandler,
		mediaTypesConfig:    s.mediaTypesConfig,
		language:            s.language,
		siteBucket:          s.siteBucket,
		h:                   s.h,
		publisher:           s.publisher,
		siteConfigConfig:    s.siteConfigConfig,
		init:                s.init,
		PageCollections:     s.PageCollections,
		siteCfg:             s.siteCfg,
	}
}

// newSite creates a new site with the given configuration.
func newSite(cfg deps.DepsCfg) (*Site, error) {
	if cfg.Language == nil {
		cfg.Language = langs.NewDefaultLanguage(cfg.Cfg)
	}
	if cfg.Logger == nil {
		panic("logger must be set")
	}

	ignoreErrors := cast.ToStringSlice(cfg.Language.Get("ignoreErrors"))
	ignorableLogger := loggers.NewIgnorableLogger(cfg.Logger, ignoreErrors...)

	disabledKinds := make(map[string]bool)
	for _, disabled := range cast.ToStringSlice(cfg.Language.Get("disableKinds")) {
		disabledKinds[disabled] = true
	}

	if disabledKinds["taxonomyTerm"] {
		// Correct from the value it had before Hugo 0.73.0.
		if disabledKinds[page.KindTaxonomy] {
			disabledKinds[page.KindTerm] = true
		} else {
			disabledKinds[page.KindTaxonomy] = true
		}

		delete(disabledKinds, "taxonomyTerm")
	} else if disabledKinds[page.KindTaxonomy] && !disabledKinds[page.KindTerm] {
		// This is a potentially ambiguous situation. It may be correct.
		ignorableLogger.Errorsf(constants.ErrIDAmbigousDisableKindTaxonomy, `You have the value 'taxonomy' in the disabledKinds list. In Hugo 0.73.0 we fixed these to be what most people expect (taxonomy and term).
But this also means that your site configuration may not do what you expect. If it is correct, you can suppress this message by following the instructions below.`)
	}

	var (
		mediaTypesConfig    []map[string]any
		outputFormatsConfig []map[string]any

		siteOutputFormatsConfig output.Formats
		siteMediaTypesConfig    media.Types
		err                     error
	)

	// Add language last, if set, so it gets precedence.
	for _, cfg := range []config.Provider{cfg.Cfg, cfg.Language} {
		if cfg.IsSet("mediaTypes") {
			mediaTypesConfig = append(mediaTypesConfig, cfg.GetStringMap("mediaTypes"))
		}
		if cfg.IsSet("outputFormats") {
			outputFormatsConfig = append(outputFormatsConfig, cfg.GetStringMap("outputFormats"))
		}
	}

	siteMediaTypesConfig, err = media.DecodeTypes(mediaTypesConfig...)
	if err != nil {
		return nil, err
	}

	siteOutputFormatsConfig, err = output.DecodeFormats(siteMediaTypesConfig, outputFormatsConfig...)
	if err != nil {
		return nil, err
	}

	rssDisabled := disabledKinds[kindRSS]
	if rssDisabled {
		// Legacy
		tmp := siteOutputFormatsConfig[:0]
		for _, x := range siteOutputFormatsConfig {
			if !strings.EqualFold(x.Name, "rss") {
				tmp = append(tmp, x)
			}
		}
		siteOutputFormatsConfig = tmp
	}

	var siteOutputs map[string]any
	if cfg.Language.IsSet("outputs") {
		siteOutputs = cfg.Language.GetStringMap("outputs")

		// Check and correct taxonomy kinds vs pre Hugo 0.73.0.
		v1, hasTaxonomyTerm := siteOutputs["taxonomyterm"]
		v2, hasTaxonomy := siteOutputs[page.KindTaxonomy]
		_, hasTerm := siteOutputs[page.KindTerm]
		if hasTaxonomy && hasTaxonomyTerm {
			siteOutputs[page.KindTaxonomy] = v1
			siteOutputs[page.KindTerm] = v2
			delete(siteOutputs, "taxonomyTerm")
		} else if hasTaxonomy && !hasTerm {
			// This is a potentially ambiguous situation. It may be correct.
			ignorableLogger.Errorsf(constants.ErrIDAmbigousOutputKindTaxonomy, `You have configured output formats for 'taxonomy' in your site configuration. In Hugo 0.73.0 we fixed these to be what most people expect (taxonomy and term).
But this also means that your site configuration may not do what you expect. If it is correct, you can suppress this message by following the instructions below.`)
		}
		if !hasTaxonomy && hasTaxonomyTerm {
			siteOutputs[page.KindTaxonomy] = v1
			delete(siteOutputs, "taxonomyterm")
		}
	}

	outputFormats, err := createSiteOutputFormats(siteOutputFormatsConfig, siteOutputs, rssDisabled)
	if err != nil {
		return nil, err
	}

	taxonomies := cfg.Language.GetStringMapString("taxonomies")

	var relatedContentConfig related.Config

	if cfg.Language.IsSet("related") {
		relatedContentConfig, err = related.DecodeConfig(cfg.Language.GetParams("related"))
		if err != nil {
			return nil, fmt.Errorf("failed to decode related config: %w", err)
		}
	} else {
		relatedContentConfig = related.DefaultConfig
		if _, found := taxonomies["tag"]; found {
			relatedContentConfig.Add(related.IndexConfig{Name: "tags", Weight: 80})
		}
	}

	titleFunc := helpers.GetTitleFunc(cfg.Language.GetString("titleCaseStyle"))

	frontMatterHandler, err := pagemeta.NewFrontmatterHandler(cfg.Logger, cfg.Cfg)
	if err != nil {
		return nil, err
	}

	timeout := 30 * time.Second
	if cfg.Language.IsSet("timeout") {
		v := cfg.Language.Get("timeout")
		d, err := types.ToDurationE(v)
		if err == nil {
			timeout = d
		}
	}

	siteConfig := siteConfigHolder{
		sitemap:          config.DecodeSitemap(config.Sitemap{Priority: -1, Filename: "sitemap.xml"}, cfg.Language.GetStringMap("sitemap")),
		taxonomiesConfig: taxonomies,
		timeout:          timeout,
		hasCJKLanguage:   cfg.Language.GetBool("hasCJKLanguage"),
		enableEmoji:      cfg.Language.Cfg.GetBool("enableEmoji"),
	}

	var siteBucket *pagesMapBucket
	if cfg.Language.IsSet("cascade") {
		var err error
		cascade, err := page.DecodeCascade(cfg.Language.Get("cascade"))
		if err != nil {
			return nil, fmt.Errorf("failed to decode cascade config: %s", err)
		}

		siteBucket = &pagesMapBucket{
			cascade: cascade,
		}

	}

	s := &Site{
		language:      cfg.Language,
		siteBucket:    siteBucket,
		disabledKinds: disabledKinds,

		outputFormats:       outputFormats,
		outputFormatsConfig: siteOutputFormatsConfig,
		mediaTypesConfig:    siteMediaTypesConfig,

		siteCfg: siteConfig,

		titleFunc: titleFunc,

		rc: &siteRenderingContext{output.HTMLFormat},

		frontmatterHandler: frontMatterHandler,
		relatedDocsHandler: page.NewRelatedDocsHandler(relatedContentConfig),
	}

	s.prepareInits()

	return s, nil
}

// NewSite creates a new site with the given dependency configuration.
// The site will have a template system loaded and ready to use.
// Note: This is mainly used in single site tests.
func NewSite(cfg deps.DepsCfg) (*Site, error) {
	s, err := newSite(cfg)
	if err != nil {
		return nil, err
	}

	var l configLoader
	if err = l.applyDeps(cfg, s); err != nil {
		return nil, err
	}

	return s, nil
}

// NewSiteDefaultLang creates a new site in the default language.
// The site will have a template system loaded and ready to use.
// Note: This is mainly used in single site tests.
// TODO(bep) test refactor -- remove
func NewSiteDefaultLang(withTemplate ...func(templ tpl.TemplateManager) error) (*Site, error) {
	l := configLoader{cfg: config.New()}
	if err := l.applyConfigDefaults(); err != nil {
		return nil, err
	}
	return newSiteForLang(langs.NewDefaultLanguage(l.cfg), withTemplate...)
}

// NewEnglishSite creates a new site in English language.
// The site will have a template system loaded and ready to use.
// Note: This is mainly used in single site tests.
// TODO(bep) test refactor -- remove
func NewEnglishSite(withTemplate ...func(templ tpl.TemplateManager) error) (*Site, error) {
	l := configLoader{cfg: config.New()}
	if err := l.applyConfigDefaults(); err != nil {
		return nil, err
	}
	return newSiteForLang(langs.NewLanguage("en", l.cfg), withTemplate...)
}

// newSiteForLang creates a new site in the given language.
func newSiteForLang(lang *langs.Language, withTemplate ...func(templ tpl.TemplateManager) error) (*Site, error) {
	withTemplates := func(templ tpl.TemplateManager) error {
		for _, wt := range withTemplate {
			if err := wt(templ); err != nil {
				return err
			}
		}
		return nil
	}

	cfg := deps.DepsCfg{WithTemplate: withTemplates, Cfg: lang}

	return NewSiteForCfg(cfg)
}

// NewSiteForCfg creates a new site for the given configuration.
// The site will have a template system loaded and ready to use.
// Note: This is mainly used in single site tests.
func NewSiteForCfg(cfg deps.DepsCfg) (*Site, error) {
	h, err := NewHugoSites(cfg)
	if err != nil {
		return nil, err
	}
	return h.Sites[0], nil
}

type SiteInfo struct {
	Authors page.AuthorList
	Social  SiteSocial

	hugoInfo     hugo.Info
	title        string
	RSSLink      string
	Author       map[string]any
	LanguageCode string
	Copyright    string

	permalinks map[string]string

	LanguagePrefix string
	Languages      langs.Languages

	BuildDrafts bool

	canonifyURLs bool
	relativeURLs bool
	uglyURLs     func(p page.Page) bool
	RemoveHTMLExtension bool

	owner                          *HugoSites
	s                              *Site
	language                       *langs.Language
	defaultContentLanguageInSubdir bool
	sectionPagesMenu               string
}

func (s *SiteInfo) Pages() page.Pages {
	return s.s.Pages()
}

func (s *SiteInfo) RegularPages() page.Pages {
	return s.s.RegularPages()
}

func (s *SiteInfo) AllPages() page.Pages {
	return s.s.AllPages()
}

func (s *SiteInfo) AllRegularPages() page.Pages {
	return s.s.AllRegularPages()
}

func (s *SiteInfo) LastChange() time.Time {
	return s.s.lastmod
}

func (s *SiteInfo) Title() string {
	return s.title
}

func (s *SiteInfo) Site() page.Site {
	return s
}

func (s *SiteInfo) Menus() navigation.Menus {
	return s.s.Menus()
}

// TODO(bep) type
func (s *SiteInfo) Taxonomies() page.TaxonomyList {
	return s.s.Taxonomies()
}

func (s *SiteInfo) Params() maps.Params {
	return s.s.Language().Params()
}

func (s *SiteInfo) Data() map[string]any {
	return s.s.h.Data()
}

func (s *SiteInfo) Language() *langs.Language {
	return s.language
}

func (s *SiteInfo) Config() SiteConfig {
	return s.s.siteConfigConfig
}

func (s *SiteInfo) Hugo() hugo.Info {
	return s.hugoInfo
}

// Sites is a convenience method to get all the Hugo sites/languages configured.
func (s *SiteInfo) Sites() page.Sites {
	return s.s.h.siteInfos()
}

// Current returns the currently rendered Site.
// If that isn't set yet, which is the situation before we start rendering,
// if will return the Site itself.
func (s *SiteInfo) Current() page.Site {
	if s.s.h.currentSite == nil {
		return s
	}
	return s.s.h.currentSite.Info
}

func (s *SiteInfo) String() string {
	return fmt.Sprintf("Site(%q)", s.title)
}

func (s *SiteInfo) BaseURL() template.URL {
	return template.URL(s.s.PathSpec.BaseURL.String())
}

// ServerPort returns the port part of the BaseURL, 0 if none found.
func (s *SiteInfo) ServerPort() int {
	ps := s.s.PathSpec.BaseURL.URL().Port()
	if ps == "" {
		return 0
	}
	p, err := strconv.Atoi(ps)
	if err != nil {
		return 0
	}
	return p
}

// GoogleAnalytics is kept here for historic reasons.
func (s *SiteInfo) GoogleAnalytics() string {
	return s.Config().Services.GoogleAnalytics.ID
}

// DisqusShortname is kept here for historic reasons.
func (s *SiteInfo) DisqusShortname() string {
	return s.Config().Services.Disqus.Shortname
}

func (s *SiteInfo) GetIdentity() identity.Identity {
	return identity.KeyValueIdentity{Key: "site", Value: s.language.Lang}
}

// SiteSocial is a place to put social details on a site level. These are the
// standard keys that themes will expect to have available, but can be
// expanded to any others on a per site basis
// github
// facebook
// facebook_admin
// twitter
// twitter_domain
// pinterest
// instagram
// youtube
// linkedin
type SiteSocial map[string]string

// Param is a convenience method to do lookups in SiteInfo's Params map.
//
// This method is also implemented on Page.
func (s *SiteInfo) Param(key any) (any, error) {
	return resource.Param(s, nil, key)
}

func (s *SiteInfo) IsMultiLingual() bool {
	return len(s.Languages) > 1
}

func (s *SiteInfo) IsServer() bool {
	return s.owner.running
}

type siteRefLinker struct {
	s *Site

	errorLogger *log.Logger
	notFoundURL string
}

func newSiteRefLinker(s *Site) (siteRefLinker, error) {
	logger := s.Log.Error()

	notFoundURL := s.conf.RefLinksNotFoundURL
	errLevel := s.conf.RefLinksErrorLevel
	if strings.EqualFold(errLevel, "warning") {
		logger = s.Log.Warn()
	}
	return siteRefLinker{s: s, errorLogger: logger, notFoundURL: notFoundURL}, nil
}

func (s siteRefLinker) logNotFound(ref, what string, p page.Page, position text.Position) {
	if position.IsValid() {
		s.errorLogger.Printf("[%s] REF_NOT_FOUND: Ref %q: %s: %s", s.s.Lang(), ref, position.String(), what)
	} else if p == nil {
		s.errorLogger.Printf("[%s] REF_NOT_FOUND: Ref %q: %s", s.s.Lang(), ref, what)
	} else {
		s.errorLogger.Printf("[%s] REF_NOT_FOUND: Ref %q from page %q: %s", s.s.Lang(), ref, p.Pathc(), what)
	}
}

func (s *siteRefLinker) refLink(ref string, source any, relative bool, outputFormat string) (string, error) {
	p, err := unwrapPage(source)
	if err != nil {
		return "", err
	}

	var refURL *url.URL

	ref = filepath.ToSlash(ref)

	refURL, err = url.Parse(ref)

	if err != nil {
		return s.notFoundURL, err
	}

	var target page.Page
	var link string

	if refURL.Path != "" {
		var err error
		target, err = s.s.getPageRef(p, refURL.Path)
		var pos text.Position
		if err != nil || target == nil {
			if p, ok := source.(text.Positioner); ok {
				pos = p.Position()
			}
		}

		if err != nil {
			s.logNotFound(refURL.Path, err.Error(), p, pos)
			return s.notFoundURL, nil
		}

		if target == nil {
			s.logNotFound(refURL.Path, "page not found", p, pos)
			return s.notFoundURL, nil
		}

		var permalinker Permalinker = target

		if outputFormat != "" {
			o := target.OutputFormats().Get(outputFormat)

			if o == nil {
				s.logNotFound(refURL.Path, fmt.Sprintf("output format %q", outputFormat), p, pos)
				return s.notFoundURL, nil
			}
			permalinker = o
		}

		if relative {
			link = permalinker.RelPermalink()
		} else {
			link = permalinker.Permalink()
		}
	}

	if refURL.Fragment != "" {
		_ = target
		link = link + "#" + refURL.Fragment

		if pctx, ok := target.(pageContext); ok {
			if refURL.Path != "" {
				if di, ok := pctx.getContentConverter().(converter.DocumentInfo); ok {
					link = link + di.AnchorSuffix()
				}
			}
		} else if pctx, ok := p.(pageContext); ok {
			if di, ok := pctx.getContentConverter().(converter.DocumentInfo); ok {
				link = link + di.AnchorSuffix()
			}
		}

	}

	return link, nil
}

func (s *Site) watching() bool {
	return s.h != nil && s.h.Configs.Base.Internal.Watch
}

type whatChanged struct {
	source bool
	files  map[string]bool
}

// RegisterMediaTypes will register the Site's media types in the mime
// package, so it will behave correctly with Hugo's built-in server.
func (s *Site) RegisterMediaTypes() {
	for _, mt := range s.conf.MediaTypes.Config {
		for _, suffix := range mt.Suffixes() {
			_ = mime.AddExtensionType(mt.Delimiter+suffix, mt.Type+"; charset=utf-8")
		}
	}
}

func (s *Site) filterFileEvents(events []fsnotify.Event) []fsnotify.Event {
	var filtered []fsnotify.Event
	seen := make(map[fsnotify.Event]bool)

	for _, ev := range events {
		// Avoid processing the same event twice.
		if seen[ev] {
			continue
		}
		seen[ev] = true

		if s.SourceSpec.IgnoreFile(ev.Name) {
			continue
		}

		// Throw away any directories
		isRegular, err := s.SourceSpec.IsRegularSourceFile(ev.Name)
		if err != nil && herrors.IsNotExist(err) && (ev.Op&fsnotify.Remove == fsnotify.Remove || ev.Op&fsnotify.Rename == fsnotify.Rename) {
			// Force keep of event
			isRegular = true
		}
		if !isRegular {
			continue
		}

		if runtime.GOOS == "darwin" { // When a file system is HFS+, its filepath is in NFD form.
			ev.Name = norm.NFC.String(ev.Name)
		}

		filtered = append(filtered, ev)
	}

	return filtered
}

func (s *Site) translateFileEvents(events []fsnotify.Event) []fsnotify.Event {
	var filtered []fsnotify.Event

	eventMap := make(map[string][]fsnotify.Event)

	// We often get a Remove etc. followed by a Create, a Create followed by a Write.
	// Remove the superfluous events to mage the update logic simpler.
	for _, ev := range events {
		eventMap[ev.Name] = append(eventMap[ev.Name], ev)
	}

	for _, ev := range events {
		mapped := eventMap[ev.Name]

		// Keep one
		found := false
		var kept fsnotify.Event
		for i, ev2 := range mapped {
			if i == 0 {
				kept = ev2
			}

			if ev2.Op&fsnotify.Write == fsnotify.Write {
				kept = ev2
				found = true
			}

			if !found && ev2.Op&fsnotify.Create == fsnotify.Create {
				kept = ev2
			}
		}

		filtered = append(filtered, kept)
	}

	return filtered
}

// reBuild partially rebuilds a site given the filesystem events.
// It returns whatever the content source was changed.
// TODO(bep) clean up/rewrite this method.
func (s *Site) processPartial(config *BuildCfg, init func(config *BuildCfg) error, events []fsnotify.Event) error {
	events = s.filterFileEvents(events)
	events = s.translateFileEvents(events)

	changeIdentities := make(identity.Identities)

	s.Log.Debugf("Rebuild for events %q", events)

	h := s.h

	// First we need to determine what changed

	var (
		sourceChanged       = []fsnotify.Event{}
		sourceReallyChanged = []fsnotify.Event{}
		contentFilesChanged []string

		tmplChanged bool
		tmplAdded   bool
		dataChanged bool
		i18nChanged bool

		sourceFilesChanged = make(map[string]bool)

		// prevent spamming the log on changes
		logger = helpers.NewDistinctErrorLogger()
	)

	var cacheBusters []func(string) bool
	bcfg := s.conf.Build

	for _, ev := range events {
		component, relFilename := s.BaseFs.MakePathRelative(ev.Name)
		if relFilename != "" {
			p := hglob.NormalizePath(path.Join(component, relFilename))
			g, err := bcfg.MatchCacheBuster(s.Log, p)
			if err == nil && g != nil {
				cacheBusters = append(cacheBusters, g)
			}
		}

		id, found := s.eventToIdentity(ev)
		if found {
			changeIdentities[id] = id

			switch id.Type {
			case files.ComponentFolderContent:
				logger.Println("Source changed", ev)
				sourceChanged = append(sourceChanged, ev)
			case files.ComponentFolderLayouts:
				tmplChanged = true
				if !s.Tmpl().HasTemplate(id.Path) {
					tmplAdded = true
				}
				if tmplAdded {
					logger.Println("Template added", ev)
				} else {
					logger.Println("Template changed", ev)
				}

			case files.ComponentFolderData:
				logger.Println("Data changed", ev)
				dataChanged = true
			case files.ComponentFolderI18n:
				logger.Println("i18n changed", ev)
				i18nChanged = true

			}
		}
	}

	changed := &whatChanged{
		source: len(sourceChanged) > 0,
		files:  sourceFilesChanged,
	}

	config.whatChanged = changed

	if err := init(config); err != nil {
		return err
	}

	var cacheBusterOr func(string) bool
	if len(cacheBusters) > 0 {
		cacheBusterOr = func(s string) bool {
			for _, cb := range cacheBusters {
				if cb(s) {
					return true
				}
			}
			return false
		}
	}

	// These in memory resource caches will be rebuilt on demand.
	if len(cacheBusters) > 0 {
		s.h.ResourceSpec.ResourceCache.DeleteMatches(cacheBusterOr)
	}

	if tmplChanged || i18nChanged {
		s.h.init.Reset()
		var prototype *deps.Deps
		for i, s := range s.h.Sites {
			if err := s.Deps.Compile(prototype); err != nil {
				return err
			}
			if i == 0 {
				prototype = s.Deps
			}
		}
	}

	if dataChanged {
		s.h.init.data.Reset()
	}

	for _, ev := range sourceChanged {
		removed := false

		if ev.Op&fsnotify.Remove == fsnotify.Remove {
			removed = true
		}

		// Some editors (Vim) sometimes issue only a Rename operation when writing an existing file
		// Sometimes a rename operation means that file has been renamed other times it means
		// it's been updated
		if ev.Op&fsnotify.Rename == fsnotify.Rename {
			// If the file is still on disk, it's only been updated, if it's not, it's been moved
			if ex, err := afero.Exists(s.Fs.Source, ev.Name); !ex || err != nil {
				removed = true
			}
		}

		if removed && files.IsContentFile(ev.Name) {
			h.removePageByFilename(ev.Name)
		}

		sourceReallyChanged = append(sourceReallyChanged, ev)
		sourceFilesChanged[ev.Name] = true
	}

	if config.ErrRecovery || tmplAdded || dataChanged {
		h.resetPageState()
	} else {
		h.resetPageStateFromEvents(changeIdentities)
	}

	if len(sourceReallyChanged) > 0 || len(contentFilesChanged) > 0 {
		var filenamesChanged []string
		for _, e := range sourceReallyChanged {
			filenamesChanged = append(filenamesChanged, e.Name)
		}
		if len(contentFilesChanged) > 0 {
			filenamesChanged = append(filenamesChanged, contentFilesChanged...)
		}

		filenamesChanged = helpers.UniqueStringsReuse(filenamesChanged)

		if err := s.readAndProcessContent(*config, filenamesChanged...); err != nil {
			return err
		}

	}

	return nil
}

func (s *Site) process(config BuildCfg) (err error) {
	if err = s.readAndProcessContent(config); err != nil {
		err = fmt.Errorf("readAndProcessContent: %w", err)
		return
	}
	return err
}

func (s *Site) render(ctx *siteRenderContext) (err error) {
	if err := page.Clear(); err != nil {
		return err
	}

	if ctx.outIdx == 0 {
		// Note that even if disableAliases is set, the aliases themselves are
		// preserved on page. The motivation with this is to be able to generate
		// 301 redirects in a .htacess file and similar using a custom output format.
		if !s.conf.DisableAliases {
			// Aliases must be rendered before pages.
			// Some sites, Hugo docs included, have faulty alias definitions that point
			// to itself or another real page. These will be overwritten in the next
			// step.
			if err = s.renderAliases(); err != nil {
				return
			}
		}
	}

	if err = s.renderPages(ctx); err != nil {
		return
	}

	if ctx.outIdx == 0 {
		if err = s.renderSitemap(); err != nil {
			return
		}

		if ctx.multihost {
			if err = s.renderRobotsTXT(); err != nil {
				return
			}
		}

		if err = s.render404(); err != nil {
			return
		}
	}

	if !ctx.renderSingletonPages() {
		return
	}

	if err = s.renderMainLanguageRedirect(); err != nil {
		return
	}

	return
}

// HomeAbsURL is a convenience method giving the absolute URL to the home page.
func (s *Site) HomeAbsURL() string {
	base := ""
	if len(s.conf.Languages) > 1 {
		base = s.Language().Lang
	}
	return s.AbsURL(base, false)
}

// SitemapAbsURL is a convenience method giving the absolute URL to the sitemap.
func (s *Site) SitemapAbsURL() string {
	p := s.HomeAbsURL()
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	p += s.conf.Sitemap.Filename
	return p
}

func (s *Site) initializeSiteInfo() error {
	var (
		lang      = s.language
		languages langs.Languages
	)

	if s.h != nil && s.h.multilingual != nil {
		languages = s.h.multilingual.Languages
	}

	permalinks := s.Cfg.GetStringMapString("permalinks")

	defaultContentInSubDir := s.Cfg.GetBool("defaultContentLanguageInSubdir")
	defaultContentLanguage := s.Cfg.GetString("defaultContentLanguage")

	languagePrefix := ""
	if s.multilingualEnabled() && (defaultContentInSubDir || lang.Lang != defaultContentLanguage) {
		languagePrefix = "/" + lang.Lang
	}

	uglyURLs := func(p page.Page) bool {
		return false
	}

	v := s.Cfg.Get("uglyURLs")
	if v != nil {
		switch vv := v.(type) {
		case bool:
			uglyURLs = func(p page.Page) bool {
				return vv
			}
		case string:
			// Is what be get from CLI (--uglyURLs)
			vvv := cast.ToBool(vv)
			uglyURLs = func(p page.Page) bool {
				return vvv
			}
		default:
			m := maps.ToStringMapBool(v)
			uglyURLs = func(p page.Page) bool {
				return m[p.Section()]
			}
		}
	}

	// Assemble dependencies to be used in hugo.Deps.
	// TODO(bep) another reminder: We need to clean up this Site vs HugoSites construct.
	var deps []*hugo.Dependency
	var depFromMod func(m modules.Module) *hugo.Dependency
	depFromMod = func(m modules.Module) *hugo.Dependency {
		dep := &hugo.Dependency{
			Path:    m.Path(),
			Version: m.Version(),
			Time:    m.Time(),
			Vendor:  m.Vendor(),
		}

		// These are pointers, but this all came from JSON so there's no recursive navigation,
		// so just create new values.
		if m.Replace() != nil {
			dep.Replace = depFromMod(m.Replace())
		}
		if m.Owner() != nil {
			dep.Owner = depFromMod(m.Owner())
		}
		return dep
	}
	for _, m := range s.Paths.AllModules {
		deps = append(deps, depFromMod(m))
	}

	s.Info = &SiteInfo{
		title:                          lang.GetString("title"),
		Author:                         lang.GetStringMap("author"),
		Social:                         lang.GetStringMapString("social"),
		LanguageCode:                   lang.GetString("languageCode"),
		Copyright:                      lang.GetString("copyright"),
		language:                       lang,
		LanguagePrefix:                 languagePrefix,
		Languages:                      languages,
		defaultContentLanguageInSubdir: defaultContentInSubDir,
		sectionPagesMenu:               lang.GetString("sectionPagesMenu"),
		BuildDrafts:                    s.Cfg.GetBool("buildDrafts"),
		canonifyURLs:                   s.Cfg.GetBool("canonifyURLs"),
		relativeURLs:                   s.Cfg.GetBool("relativeURLs"),
		uglyURLs:                       uglyURLs,
		RemoveHTMLExtension:            s.Cfg.GetBool("removeHTMLExtension"),
		permalinks:                     permalinks,
		owner:                          s.h,
		s:                              s,
		hugoInfo:                       hugo.NewInfo(s.Cfg.GetString("environment"), deps),
	}

	rssOutputFormat, found := s.outputFormats[page.KindHome].GetByName(output.RSSFormat.Name)

	if found {
		s.Info.RSSLink = s.permalink(rssOutputFormat.BaseFilename())
	}

	return nil
}

func (s *Site) eventToIdentity(e fsnotify.Event) (identity.PathIdentity, bool) {
	for _, fs := range s.BaseFs.SourceFilesystems.FileSystems() {
		if p := fs.Path(e.Name); p != "" {
			return identity.NewPathIdentity(fs.Name, filepath.ToSlash(p)), true
		}
	}
	return identity.PathIdentity{}, false
}

func (s *Site) readAndProcessContent(buildConfig BuildCfg, filenames ...string) error {
	if s.Deps == nil {
		panic("nil deps on site")
	}

	sourceSpec := source.NewSourceSpec(s.PathSpec, buildConfig.ContentInclusionFilter, s.BaseFs.Content.Fs)

	proc := newPagesProcessor(s.h, sourceSpec)

	c := newPagesCollector(sourceSpec, s.h.getContentMaps(), s.Log, s.h.ContentChanges, proc, filenames...)

	if err := c.Collect(); err != nil {
		return err
	}

	return nil
}

func (s *Site) createNodeMenuEntryURL(in string) string {
	if !strings.HasPrefix(in, "/") {
		return in
	}
	// make it match the nodes
	menuEntryURL := in
	menuEntryURL = helpers.SanitizeURLKeepTrailingSlash(s.s.PathSpec.URLize(menuEntryURL))
	if !s.conf.CanonifyURLs {
		menuEntryURL = paths.AddContextRoot(s.s.PathSpec.Cfg.BaseURL().String(), menuEntryURL)
	}
	return menuEntryURL
}

func (s *Site) assembleMenus() {
	s.menus = make(navigation.Menus)

	type twoD struct {
		MenuName, EntryName string
	}
	flat := map[twoD]*navigation.MenuEntry{}
	children := map[twoD]navigation.Menu{}

	// add menu entries from config to flat hash
	for name, menu := range s.conf.Menus.Config {
		for _, me := range menu {
			if types.IsNil(me.Page) {
				if me.PageRef != "" {
					// Try to resolve the page.
					p, _ := s.getPageNew(nil, me.PageRef)
					if !types.IsNil(p) {
						navigation.SetPageValues(me, p)
					}
				}

			} else {
				me.ConfiguredURL = s.createNodeMenuEntryURL(me.MenuConfig.URL)
			}

			flat[twoD{name, me.KeyName()}] = me
		}
	}

	sectionPagesMenu := s.conf.SectionPagesMenu

	if sectionPagesMenu != "" {
		s.pageMap.sections.Walk(func(s string, v any) bool {
			p := v.(*contentNode).p
			if p.IsHome() {
				return false
			}
			// From Hugo 0.22 we have nested sections, but until we get a
			// feel of how that would work in this setting, let us keep
			// this menu for the top level only.
			id := p.Section()
			if _, ok := flat[twoD{sectionPagesMenu, id}]; ok {
				return false
			}

			me := navigation.MenuEntry{
				MenuConfig: navigation.MenuConfig{
					Identifier: id,
					Name:       p.LinkTitle(),
					Weight:     p.Weight(),
				},
			}
			navigation.SetPageValues(&me, p)
			flat[twoD{sectionPagesMenu, me.KeyName()}] = &me

			return false
		})
	}

	// Add menu entries provided by pages
	s.pageMap.pageTrees.WalkRenderable(func(ss string, n *contentNode) bool {
		p := n.p

		for name, me := range p.pageMenus.menus() {
			if _, ok := flat[twoD{name, me.KeyName()}]; ok {
				err := p.wrapError(fmt.Errorf("duplicate menu entry with identifier %q in menu %q", me.KeyName(), name))
				s.Log.Warnln(err)
				continue
			}
			flat[twoD{name, me.KeyName()}] = me
		}

		return false
	})

	// Create Children Menus First
	for _, e := range flat {
		if e.Parent != "" {
			children[twoD{e.Menu, e.Parent}] = children[twoD{e.Menu, e.Parent}].Add(e)
		}
	}

	// Placing Children in Parents (in flat)
	for p, childmenu := range children {
		_, ok := flat[twoD{p.MenuName, p.EntryName}]
		if !ok {
			// if parent does not exist, create one without a URL
			flat[twoD{p.MenuName, p.EntryName}] = &navigation.MenuEntry{
				MenuConfig: navigation.MenuConfig{
					Name: p.EntryName,
				},
			}
		}
		flat[twoD{p.MenuName, p.EntryName}].Children = childmenu
	}

	// Assembling Top Level of Tree
	for menu, e := range flat {
		if e.Parent == "" {
			_, ok := s.menus[menu.MenuName]
			if !ok {
				s.menus[menu.MenuName] = navigation.Menu{}
			}
			s.menus[menu.MenuName] = s.menus[menu.MenuName].Add(e)
		}
	}
}

// get any language code to prefix the target file path with.
func (s *Site) getLanguageTargetPathLang(alwaysInSubDir bool) string {
	if s.h.Conf.IsMultihost() {
		return s.Language().Lang
	}

	return s.getLanguagePermalinkLang(alwaysInSubDir)
}

// get any lanaguagecode to prefix the relative permalink with.
func (s *Site) getLanguagePermalinkLang(alwaysInSubDir bool) string {
	if !s.h.isMultiLingual() || s.h.Conf.IsMultihost() {
		return ""
	}

	if alwaysInSubDir {
		return s.Language().Lang
	}

	isDefault := s.Language().Lang == s.conf.DefaultContentLanguage

	if !isDefault || s.conf.DefaultContentLanguageInSubdir {
		return s.Language().Lang
	}

	return ""
}

func (s *Site) getTaxonomyKey(key string) string {
	if s.conf.DisablePathToLower {
		return s.PathSpec.MakePath(key)
	}
	return strings.ToLower(s.PathSpec.MakePath(key))
}

// Prepare site for a new full build.
func (s *Site) resetBuildState(sourceChanged bool) {
	s.relatedDocsHandler = s.relatedDocsHandler.Clone()
	s.init.Reset()

	if sourceChanged {
		s.pageMap.contentMap.pageReverseIndex.Reset()
		s.PageCollections = newPageCollections(s.pageMap)
		s.pageMap.withEveryBundlePage(func(p *pageState) bool {
			p.pagePages = &pagePages{}
			if p.bucket != nil {
				p.bucket.pagesMapBucketPages = &pagesMapBucketPages{}
			}
			p.parent = nil
			p.Scratcher = maps.NewScratcher()
			return false
		})
	} else {
		s.pageMap.withEveryBundlePage(func(p *pageState) bool {
			p.Scratcher = maps.NewScratcher()
			return false
		})
	}
}

func (s *Site) errorCollator(results <-chan error, errs chan<- error) {
	var errors []error
	for e := range results {
		errors = append(errors, e)
	}

	errs <- s.h.pickOneAndLogTheRest(errors)

	close(errs)
}

// GetPage looks up a page of a given type for the given ref.
// In Hugo <= 0.44 you had to add Page Kind (section, home) etc. as the first
// argument and then either a unix styled path (with or without a leading slash))
// or path elements separated.
// When we now remove the Kind from this API, we need to make the transition as painless
// as possible for existing sites. Most sites will use {{ .Site.GetPage "section" "my/section" }},
// i.e. 2 arguments, so we test for that.
func (s *Site) GetPage(ref ...string) (page.Page, error) {
	p, err := s.s.getPageOldVersion(ref...)

	if p == nil {
		// The nil struct has meaning in some situations, mostly to avoid breaking
		// existing sites doing $nilpage.IsDescendant($p), which will always return
		// false.
		p = page.NilPage
	}

	return p, err
}

func (s *Site) GetPageWithTemplateInfo(info tpl.Info, ref ...string) (page.Page, error) {
	p, err := s.GetPage(ref...)
	if p != nil {
		// Track pages referenced by templates/shortcodes
		// when in server mode.
		if im, ok := info.(identity.Manager); ok {
			im.Add(p)
		}
	}
	return p, err
}

func (s *Site) permalink(link string) string {
	return s.PathSpec.PermalinkForBaseURL(link, s.PathSpec.Cfg.BaseURL().String())
}

func (s *Site) absURLPath(targetPath string) string {
	var path string
	if s.conf.RelativeURLs {
		path = helpers.GetDottedRelativePath(targetPath)
	} else {
		url := s.PathSpec.Cfg.BaseURL().String()
		if !strings.HasSuffix(url, "/") {
			url += "/"
		}
		path = url
	}

	return path
}

func (s *Site) lookupLayouts(layouts ...string) tpl.Template {
	for _, l := range layouts {
		if templ, found := s.Tmpl().Lookup(l); found {
			return templ
		}
	}

	return nil
}

func (s *Site) renderAndWriteXML(ctx context.Context, statCounter *uint64, name string, targetPath string, d any, templ tpl.Template) error {
	renderBuffer := bp.GetBuffer()
	defer bp.PutBuffer(renderBuffer)

	if err := s.renderForTemplate(ctx, name, "", d, renderBuffer, templ); err != nil {
		return err
	}

	pd := publisher.Descriptor{
		Src:         renderBuffer,
		TargetPath:  targetPath,
		StatCounter: statCounter,
		// For the minification part of XML,
		// we currently only use the MIME type.
		OutputFormat: output.RSSFormat,
		AbsURLPath:   s.absURLPath(targetPath),
	}

	return s.publisher.Publish(pd)
}

func (s *Site) renderAndWritePage(statCounter *uint64, name string, targetPath string, p *pageState, templ tpl.Template) error {
	s.h.IncrPageRender()
	renderBuffer := bp.GetBuffer()
	defer bp.PutBuffer(renderBuffer)

	of := p.outputFormat()
	ctx := tpl.SetPageInContext(context.Background(), p)

	if err := s.renderForTemplate(ctx, p.Kind(), of.Name, p, renderBuffer, templ); err != nil {
		return err
	}

	if renderBuffer.Len() == 0 {
		return nil
	}

	isHTML := of.IsHTML
	isRSS := of.Name == "rss"

	pd := publisher.Descriptor{
		Src:          renderBuffer,
		TargetPath:   targetPath,
		StatCounter:  statCounter,
		OutputFormat: p.outputFormat(),
	}

	if isRSS {
		// Always canonify URLs in RSS
		pd.AbsURLPath = s.absURLPath(targetPath)
	} else if isHTML {
		if s.conf.RelativeURLs || s.conf.CanonifyURLs {
			pd.AbsURLPath = s.absURLPath(targetPath)
		}

		if s.watching() && s.conf.Internal.Watch && !s.conf.Internal.DisableLiveReload {
			pd.LiveReloadBaseURL = s.Conf.BaseURLLiveReload().URL()
		}

		// For performance reasons we only inject the Hugo generator tag on the home page.
		if p.IsHome() {
			pd.AddHugoGeneratorTag = !s.conf.DisableHugoGeneratorInject
		}

	}

	return s.publisher.Publish(pd)
}

var infoOnMissingLayout = map[string]bool{
	// The 404 layout is very much optional in Hugo, but we do look for it.
	"404": true,
}

// hookRendererTemplate is the canonical implementation of all hooks.ITEMRenderer,
// where ITEM is the thing being hooked.
type hookRendererTemplate struct {
	templateHandler tpl.TemplateHandler
	identity.SearchProvider
	templ           tpl.Template
	resolvePosition func(ctx any) text.Position
}

func (hr hookRendererTemplate) RenderLink(cctx context.Context, w io.Writer, ctx hooks.LinkContext) error {
	return hr.templateHandler.ExecuteWithContext(cctx, hr.templ, w, ctx)
}

func (hr hookRendererTemplate) RenderHeading(cctx context.Context, w io.Writer, ctx hooks.HeadingContext) error {
	return hr.templateHandler.ExecuteWithContext(cctx, hr.templ, w, ctx)
}

func (hr hookRendererTemplate) RenderCodeblock(cctx context.Context, w hugio.FlexiWriter, ctx hooks.CodeblockContext) error {
	return hr.templateHandler.ExecuteWithContext(cctx, hr.templ, w, ctx)
}

func (hr hookRendererTemplate) ResolvePosition(ctx any) text.Position {
	return hr.resolvePosition(ctx)
}

func (hr hookRendererTemplate) IsDefaultCodeBlockRenderer() bool {
	return false
}

func (s *Site) renderForTemplate(ctx context.Context, name, outputFormat string, d any, w io.Writer, templ tpl.Template) (err error) {
	if templ == nil {
		s.logMissingLayout(name, "", "", outputFormat)
		return nil
	}

	if ctx == nil {
		panic("nil context")
	}

	if err = s.Tmpl().ExecuteWithContext(ctx, templ, w, d); err != nil {
		return fmt.Errorf("render of %q failed: %w", name, err)
	}
	return
}

func (s *Site) lookupTemplate(layouts ...string) (tpl.Template, bool) {
	for _, l := range layouts {
		if templ, found := s.Tmpl().Lookup(l); found {
			return templ, true
		}
	}

	return nil, false
}

func (s *Site) publish(statCounter *uint64, path string, r io.Reader, fs afero.Fs) (err error) {
	s.PathSpec.ProcessingStats.Incr(statCounter)

	return helpers.WriteToDisk(filepath.Clean(path), r, fs)
}

func (s *Site) kindFromFileInfoOrSections(fi *fileInfo, sections []string) string {
	if fi.TranslationBaseName() == "_index" {
		if fi.Dir() == "" {
			return page.KindHome
		}

		return s.kindFromSections(sections)

	}

	return page.KindPage
}

func (s *Site) kindFromSections(sections []string) string {
	if len(sections) == 0 {
		return page.KindHome
	}

	return s.kindFromSectionPath(path.Join(sections...))
}

func (s *Site) kindFromSectionPath(sectionPath string) string {
	var taxonomiesConfig taxonomiesConfig = s.conf.Taxonomies
	for _, plural := range taxonomiesConfig {
		if plural == sectionPath {
			return page.KindTaxonomy
		}

		if strings.HasPrefix(sectionPath, plural) {
			return page.KindTerm
		}

	}

	return page.KindSection
}

func (s *Site) newPage(
	n *contentNode,
	parentbBucket *pagesMapBucket,
	kind, title string,
	sections ...string) *pageState {
	m := map[string]any{}
	if title != "" {
		m["title"] = title
	}

	p, err := newPageFromMeta(
		n,
		parentbBucket,
		m,
		&pageMeta{
			s:        s,
			kind:     kind,
			sections: sections,
		})
	if err != nil {
		panic(err)
	}

	return p
}

func (s *Site) shouldBuild(p page.Page) bool {
	return shouldBuild(s.Conf.BuildFuture(), s.Conf.BuildExpired(),
		s.Conf.BuildDrafts(), p.Draft(), p.PublishDate(), p.ExpiryDate())
}

func shouldBuild(buildFuture bool, buildExpired bool, buildDrafts bool, Draft bool,
	publishDate time.Time, expiryDate time.Time) bool {
	if !(buildDrafts || !Draft) {
		return false
	}
	hnow := htime.Now()
	if !buildFuture && !publishDate.IsZero() && publishDate.After(hnow) {
		return false
	}
	if !buildExpired && !expiryDate.IsZero() && expiryDate.Before(hnow) {
		return false
	}
	return true
}
