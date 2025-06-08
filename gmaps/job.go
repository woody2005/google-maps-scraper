package gmaps

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/google/uuid"
	"github.com/gosom/google-maps-scraper/deduper"
	"github.com/gosom/google-maps-scraper/exiter"
	"github.com/gosom/scrapemate"
	"github.com/playwright-community/playwright-go"
)

type GmapJobOptions func(*GmapJob)

type GmapJob struct {
	scrapemate.Job

	MaxDepth     int
	LangCode     string
	ExtractEmail bool

	Deduper             deduper.Deduper
	ExitMonitor         exiter.Exiter
	ExtractExtraReviews bool
}

func NewGmapJob(
	id, langCode, query string,
	maxDepth int,
	extractEmail bool,
	geoCoordinates string,
	zoom int,
	opts ...GmapJobOptions,
) *GmapJob {
	query = url.QueryEscape(query)

	const (
		maxRetries = 3
		prio       = scrapemate.PriorityLow
	)

	if id == "" {
		id = uuid.New().String()
	}

	mapURL := ""
	if geoCoordinates != "" && zoom > 0 {
		mapURL = fmt.Sprintf("https://www.google.com/maps/search/%s/@%s,%dz", query, strings.ReplaceAll(geoCoordinates, " ", ""), zoom)
	} else {
		//Warning: geo and zoom MUST be both set or not
		mapURL = fmt.Sprintf("https://www.google.com/maps/search/%s", query)
	}

	job := GmapJob{
		Job: scrapemate.Job{
			ID:         id,
			Method:     http.MethodGet,
			URL:        mapURL,
			URLParams:  map[string]string{"hl": langCode},
			MaxRetries: maxRetries,
			Priority:   prio,
		},
		MaxDepth:     maxDepth,
		LangCode:     langCode,
		ExtractEmail: extractEmail,
	}

	for _, opt := range opts {
		opt(&job)
	}

	return &job
}

func WithDeduper(d deduper.Deduper) GmapJobOptions {
	return func(j *GmapJob) {
		j.Deduper = d
	}
}

func WithExitMonitor(e exiter.Exiter) GmapJobOptions {
	return func(j *GmapJob) {
		j.ExitMonitor = e
	}
}

func WithExtraReviews() GmapJobOptions {
	return func(j *GmapJob) {
		j.ExtractExtraReviews = true
	}
}

func (j *GmapJob) UseInResults() bool {
	return false
}

func (j *GmapJob) Process(ctx context.Context, resp *scrapemate.Response) (any, []scrapemate.IJob, error) {
	defer func() {
		resp.Document = nil
		resp.Body = nil
	}()

	log := scrapemate.GetLoggerFromContext(ctx)

	doc, ok := resp.Document.(*goquery.Document)
	if !ok {
		return nil, nil, fmt.Errorf("could not convert to goquery document")
	}

	var next []scrapemate.IJob

	if strings.Contains(resp.URL, "/maps/place/") {
		jopts := []PlaceJobOptions{}
		if j.ExitMonitor != nil {
			jopts = append(jopts, WithPlaceJobExitMonitor(j.ExitMonitor))
		}

		placeJob := NewPlaceJob(j.ID, j.LangCode, resp.URL, j.ExtractEmail, j.ExtractExtraReviews, jopts...)

		next = append(next, placeJob)
	} else {
		doc.Find(`div[role=feed] div[jsaction]>a`).Each(func(_ int, s *goquery.Selection) {
			if href := s.AttrOr("href", ""); href != "" {
				jopts := []PlaceJobOptions{}
				if j.ExitMonitor != nil {
					jopts = append(jopts, WithPlaceJobExitMonitor(j.ExitMonitor))
				}

				nextJob := NewPlaceJob(j.ID, j.LangCode, href, j.ExtractEmail, j.ExtractExtraReviews, jopts...)

				if j.Deduper == nil || j.Deduper.AddIfNotExists(ctx, href) {
					next = append(next, nextJob)
				}
			}
		})
	}

	if j.ExitMonitor != nil {
		j.ExitMonitor.IncrPlacesFound(len(next))
		j.ExitMonitor.IncrSeedCompleted(1)
	}

	log.Info(fmt.Sprintf("%d places found", len(next)))

	return nil, next, nil
}

func (j *GmapJob) BrowserActions(ctx context.Context, page playwright.Page) scrapemate.Response {
	log := scrapemate.GetLoggerFromContext(ctx)
	log.Info(fmt.Sprintf("[GmapJob %s] BrowserActions started for URL: %s", j.ID, j.GetFullURL()))

	var resp scrapemate.Response

	log.Info(fmt.Sprintf("[GmapJob %s] Attempting to navigate to page", j.ID))
	pageResponse, err := page.Goto(j.GetFullURL(), playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	})

	if err != nil {
		log.Error(fmt.Sprintf("[GmapJob %s] Navigation failed: %v", j.ID, err))
		resp.Error = err
		return resp
	}
	log.Info(fmt.Sprintf("[GmapJob %s] Navigation successful", j.ID))

	log.Info(fmt.Sprintf("[GmapJob %s] Attempting to click reject cookies if required", j.ID))
	if err = clickRejectCookiesIfRequired(page); err != nil {
		log.Error(fmt.Sprintf("[GmapJob %s] Clicking reject cookies failed: %v", j.ID, err))
		resp.Error = err
		return resp
	}
	log.Info(fmt.Sprintf("[GmapJob %s] Click reject cookies successful (or no action needed)", j.ID))

	const defaultTimeout = 5000

	log.Info(fmt.Sprintf("[GmapJob %s] Waiting for URL to stabilize", j.ID))
	err = page.WaitForURL(page.URL(), playwright.PageWaitForURLOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(defaultTimeout),
	})

	if err != nil {
		log.Error(fmt.Sprintf("[GmapJob %s] Waiting for URL failed: %v", j.ID, err))
		resp.Error = err
		return resp
	}
	log.Info(fmt.Sprintf("[GmapJob %s] URL stabilized", j.ID))

	resp.URL = pageResponse.URL()
	resp.StatusCode = pageResponse.Status()
	resp.Headers = make(http.Header, len(pageResponse.Headers()))

	for k, v := range pageResponse.Headers() {
		resp.Headers.Add(k, v)
	}

	// When Google Maps finds only 1 place, it slowly redirects to that place's URL
	// check element scroll
	sel := `div[role='feed']`

	log.Info(fmt.Sprintf("[GmapJob %s] Waiting for feed selector: %s", j.ID, sel))
	//nolint:staticcheck // TODO replace with the new playwright API
	_, err = page.WaitForSelector(sel, playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(700),
	})

	var singlePlace bool

	if err != nil {
		log.Info(fmt.Sprintf("[GmapJob %s] Feed selector not found, checking for single place redirect", j.ID))
		waitCtx, waitCancel := context.WithTimeout(ctx, time.Second*5)
		defer waitCancel()

		singlePlace = waitUntilURLContains(waitCtx, page, "/maps/place/")

		waitCancel()
		if singlePlace {
			log.Info(fmt.Sprintf("[GmapJob %s] Single place redirect detected to URL: %s", j.ID, page.URL()))
		}
	}

	if singlePlace {
		resp.URL = page.URL()

		var body string
		log.Info(fmt.Sprintf("[GmapJob %s] Getting page content for single place", j.ID))
		body, err = page.Content()
		if err != nil {
			log.Error(fmt.Sprintf("[GmapJob %s] Getting page content for single place failed: %v", j.ID, err))
			resp.Error = err
			return resp
		}
		log.Info(fmt.Sprintf("[GmapJob %s] Got page content for single place", j.ID))
		resp.Body = []byte(body)
		log.Info(fmt.Sprintf("[GmapJob %s] BrowserActions finished for single place", j.ID))
		return resp
	}

	scrollSelector := `div[role='feed']`

	// Set a more aggressive timeout for the scrolling part.
	const scrollOperationTimeout = 15000 // 15 seconds
	// SetDefaultTimeout affects Evaluate, which is used in scroll()
	page.SetDefaultTimeout(float64(scrollOperationTimeout))
	log.Info(fmt.Sprintf("[GmapJob %s] Set page default timeout to %dms for scrolling operation", j.ID, scrollOperationTimeout))

	defer func() {
		// Restore to Playwright's own default timeout (30 seconds)
		page.SetDefaultTimeout(30000)
		log.Info(fmt.Sprintf("[GmapJob %s] Restored page default timeout to 30000ms (Playwright default)", j.ID))
	}()

	log.Info(fmt.Sprintf("[GmapJob %s] Starting scroll operation with maxDepth: %d", j.ID, j.MaxDepth))
	scrollCount, err := scroll(ctx, page, j.MaxDepth, scrollSelector)
	if err != nil {
		log.Error(fmt.Sprintf("[GmapJob %s] Scroll operation failed after %d scrolls: %v", j.ID, scrollCount, err))
		resp.Error = err
		return resp
	}
	log.Info(fmt.Sprintf("[GmapJob %s] Scroll operation finished. Total scrolls: %d", j.ID, scrollCount))

	log.Info(fmt.Sprintf("[GmapJob %s] Getting page content after scroll", j.ID))
	body, err := page.Content()
	if err != nil {
		log.Error(fmt.Sprintf("[GmapJob %s] Getting page content after scroll failed: %v", j.ID, err))
		resp.Error = err
		return resp
	}
	log.Info(fmt.Sprintf("[GmapJob %s] Got page content after scroll", j.ID))
	resp.Body = []byte(body)

	log.Info(fmt.Sprintf("[GmapJob %s] BrowserActions finished successfully", j.ID))
	return resp
}

func waitUntilURLContains(ctx context.Context, page playwright.Page, s string) bool {
	log := scrapemate.GetLoggerFromContext(ctx)
	ticker := time.NewTicker(time.Millisecond * 150)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info(fmt.Sprintf("waitUntilURLContains context timed out while waiting for URL to contain: %s", s))
			return false
		case <-ticker.C:
			if strings.Contains(page.URL(), s) {
				log.Info(fmt.Sprintf("waitUntilURLContains found target string '%s' in URL: %s", s, page.URL()))
				return true
			}
		}
	}
}

func clickRejectCookiesIfRequired(page playwright.Page) error {
	// click the cookie reject button if exists
	// Logger cannot be obtained here as context is not available. Logging is done in the caller.
	sel := `form[action="https://consent.google.com/save"]:first-of-type button:first-of-type`

	const timeout = 500

	//nolint:staticcheck // TODO replace with the new playwright API
	el, err := page.WaitForSelector(sel, playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(timeout),
	})

	if err != nil {
		return nil
	}

	if el == nil {
		return nil
	}

	//nolint:staticcheck // TODO replace with the new playwright API
	return el.Click()
}

func scroll(ctx context.Context,
	page playwright.Page,
	maxDepth int,
	scrollSelector string,
) (int, error) {
	log := scrapemate.GetLoggerFromContext(ctx)
	log.Info(fmt.Sprintf("scroll started with maxDepth: %d, scrollSelector: %s", maxDepth, scrollSelector))

	expr := `async () => {
		const el = document.querySelector("` + scrollSelector + `");
		el.scrollTop = el.scrollHeight;

		return new Promise((resolve, reject) => {
  			setTimeout(() => {
    		resolve(el.scrollHeight);
  			}, %d);
		});
	}`

	var currentScrollHeight int
	// Scroll to the bottom of the page.
	waitTime := 100.
	cnt := 0

	const (
		timeout  = 500
		maxWait2 = 2000
	)

	for i := 0; i < maxDepth; i++ {
		cnt++
		waitTime2 := timeout * cnt

		if waitTime2 > timeout {
			waitTime2 = maxWait2
		}
		log.Info(fmt.Sprintf("scroll attempt %d, currentScrollHeight before eval: %d, waitTime2 for JS promise: %d", i, currentScrollHeight, waitTime2))
		// Scroll to the bottom of the page.
		scrollHeight, err := page.Evaluate(fmt.Sprintf(expr, waitTime2))
		if err != nil {
			log.Error(fmt.Sprintf("scroll attempt %d failed during page.Evaluate: %v", i, err))
			return cnt, err
		}

		height, ok := scrollHeight.(int)
		if !ok {
			err := fmt.Errorf("scrollHeight is not an int: %v", scrollHeight)
			log.Error(fmt.Sprintf("scroll attempt %d failed: %v", i, err))
			return cnt, err
		}
		log.Info(fmt.Sprintf("scroll attempt %d, height returned from page.Evaluate: %d", i, height))

		if height == currentScrollHeight {
			log.Info(fmt.Sprintf("scroll attempt %d, height %d == currentScrollHeight %d, breaking scroll loop", i, height, currentScrollHeight))
			break
		}

		currentScrollHeight = height

		select {
		case <-ctx.Done():
			log.Info(fmt.Sprintf("scroll context done during attempt %d", i))
			return cnt, ctx.Err()
		default:
		}

		waitTime *= 1.5

		if waitTime > maxWait2 {
			waitTime = maxWait2
		}
		log.Info(fmt.Sprintf("scroll attempt %d, waitTime for page.WaitForTimeout: %f", i, waitTime))
		//nolint:staticcheck // TODO replace with the new playwright API
		page.WaitForTimeout(waitTime)
	}
	log.Info(fmt.Sprintf("scroll finished, total scrolls: %d", cnt))
	return cnt, nil
}
