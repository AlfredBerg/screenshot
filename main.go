package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

func main() {
	var overwrite bool
	flag.BoolVar(&overwrite, "overwrite", false, "overwrite output files when they exist")
	flag.BoolVar(&overwrite, "w", false, "overwrite output files when they exist")
	var output string
	flag.StringVar(&output, "output", "out", "output directory")
	flag.StringVar(&output, "o", "out", "output directory")
	var inFile string
	flag.StringVar(&inFile, "input", "input", "input file if stdin is not used")
	flag.StringVar(&inFile, "i", "input", "input file if stdin is not used")
	var concurrency int
	flag.IntVar(&concurrency, "concurrency", 2, "concurrency level")
	flag.IntVar(&concurrency, "c", 2, "concurrency level")
	var visible bool
	flag.BoolVar(&visible, "visible", false, "If true, won't use headless")
	flag.BoolVar(&visible, "v", false, "If true, won't use headless")
	var cpuprofile string
	flag.StringVar(&cpuprofile, "profile", "", "File to save CPU profile of program in.")
	flag.StringVar(&cpuprofile, "p", "", "File to save CPU profile of program in")

	flag.Parse()

	if cpuprofile != "" {
		f, err := os.Create(cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.DisableGPU,
		chromedp.NoDefaultBrowserCheck,

		chromedp.Flag("ignore-certificate-errors", true),
	)
	opts = append(opts, chromedp.Flag("headless", !visible))

	allocCtx, execCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer execCancel()

	var pcancel context.CancelFunc
	pctx, pcancel := chromedp.NewContext(allocCtx)
	defer pcancel()

	// start the browser to ensure we end up making new tabs in an
	// existing browser instead of making a new browser each time.
	// see: https://godoc.org/github.com/chromedp/chromedp#NewContext
	if err := chromedp.Run(pctx); err != nil {
		fmt.Fprintf(os.Stderr, "error starting browser: %s\n", err)
		return
	}

	createOutputDir(output)

	var sc *bufio.Scanner
	if inFile != "" {
		file, err := os.Open(inFile)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()

		sc = bufio.NewScanner(file)
	} else {
		sc = bufio.NewScanner(os.Stdin)
	}

	var wg sync.WaitGroup
	jobs := make(chan string)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			for requestURL := range jobs {
				ctx, cancel := context.WithTimeout(pctx, time.Second*20)
				defer cancel()

				ctx, _ = chromedp.NewContext(ctx)

				var buf []byte
				err := chromedp.Run(
					ctx,
					fullScreenshot(requestURL, 90, &buf),
				)
				if err != nil {
					handleError(err, requestURL)
					continue
				}

				path, err := makeFilepath(output, requestURL)
				if err != nil {
					handleError(err, requestURL)
					continue
				}

				if err := ioutil.WriteFile(path+".png", buf, 0644); err != nil {
					handleError(err, requestURL)
					continue
				}

			}
			wg.Done()
		}()
	}
	for sc.Scan() {
		fmt.Println(sc.Text())
		jobs <- sc.Text()
	}
	close(jobs)
	wg.Wait()

}

func handleError(err error, errorContextInfo string) {
	fmt.Fprintf(os.Stderr, "run error: %s ------ %s\n", err, errorContextInfo)

	var errorLog = fmt.Sprintf("run error: %s ------ %s\n", err, errorContextInfo)
	errorLogdata := []string{errorLog}
	err = writeDataFile(errorLogdata, "errorLog.txt", true)
}

func makeFilepath(prefix, requestURL string) (string, error) {
	u, err := url.Parse(requestURL)
	if err != nil {
		return "", err
	}
	requestPath := u.EscapedPath()

	if requestPath == "/" {
		requestPath = "/index"
	}

	re := regexp.MustCompile("[^a-zA-Z0-9_.%-]")
	requestPath = re.ReplaceAllString(requestPath, "-")

	savePath := fmt.Sprintf("%s/%s%s", prefix, u.Hostname(), requestPath)

	re = regexp.MustCompile("[^a-zA-Z0-9_.%/-]")
	savePath = re.ReplaceAllString(savePath, "-")
	// remove multiple dashes in a row
	re = regexp.MustCompile("-+")
	savePath = re.ReplaceAllString(savePath, "-")
	// remove multiple slashes in a row
	re = regexp.MustCompile("/+")
	savePath = re.ReplaceAllString(savePath, "/")
	savePath = strings.TrimSuffix(savePath, "/")
	return savePath, nil
}
func saveMeta(path string, parentURL string, ev *fetch.EventRequestPaused) error {
	b := &bytes.Buffer{}
	fmt.Fprintf(b, "url: %s\n", ev.Request.URL)
	fmt.Fprintf(b, "parent: %s\n", parentURL)
	fmt.Fprintf(b, "method: %s\n", ev.Request.Method)
	fmt.Fprintf(b, "type: %s\n", ev.ResourceType)
	b.WriteRune('\n')
	for k, v := range ev.Request.Headers {
		fmt.Fprintf(b, "> %s: %s\n", k, v)
	}
	if ev.Request.PostData != "" {
		fmt.Fprintf(b, "\n%s\n", ev.Request.PostData)
	}
	b.WriteRune('\n')
	for _, h := range ev.ResponseHeaders {
		fmt.Fprintf(b, "< %s: %s\n", h.Name, h.Value)
	}

	return ioutil.WriteFile(path, b.Bytes(), 0644)
}

func createOutputDir(output string) error {
	dir := filepath.Dir(output + "/")
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return err
	}
	return nil
}

func writeDataFile(inData []string, path string, append bool) error {
	var file *os.File
	var err error
	file, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
	if append {
		file, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	}
	if err != nil {
		return err
	}
	datawriter := bufio.NewWriter(file)
	for _, data := range inData {
		datawriter.WriteString(data + "\n")
	}
	datawriter.Flush()
	file.Close()
	return nil
}

// fullScreenshot takes a screenshot of the entire browser viewport.
//
// Liberally copied from puppeteer's source.
//
// Note: this will override the viewport emulation settings.
func fullScreenshot(urlstr string, quality int64, res *[]byte) chromedp.Tasks {
	return chromedp.Tasks{
		chromedp.Navigate(urlstr),
		chromedp.ActionFunc(func(ctx context.Context) error {

			//width, height := int64(math.Ceil(contentSize.Width)), int64(math.Ceil(contentSize.Height))
			width := int64(1920)
			height := int64(1080)

			// force viewport emulation
			err := emulation.SetDeviceMetricsOverride(width, height, 1, false).
				WithScreenOrientation(&emulation.ScreenOrientation{
					Type:  emulation.OrientationTypeLandscapePrimary,
					Angle: 0,
				}).
				Do(ctx)
			if err != nil {
				return err
			}

			// capture screenshot
			*res, err = page.CaptureScreenshot().
				WithQuality(quality).
				WithClip(&page.Viewport{
					X:      0,
					Y:      0,
					Width:  float64(width),
					Height: float64(height),
					Scale:  1,
				}).Do(ctx)

			if err != nil {
				return err
			}
			return nil
		}),
	}
}
