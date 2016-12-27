// url := "https://homebrew.bintray.com/bottles/youtube-dl-2016.12.12.sierra.bottle.tar.gz"
// url := "https://homebrew.bintray.com/bottles/libtiff-4.0.7.sierra.bottle.tar.gz"
// url := "http://127.0.0.1:8080/libtiff-4.0.7.sierra.bottle.tar.gz"
// url := "http://127.0.0.1:8080/orig.txt"
// url := "https://swtch.com/~rsc/thread/squint.pdf"
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/jessevdk/go-flags"
	"github.com/pkg/errors"
	"github.com/vbauerster/mpb"
)

const (
	maxRedirects = 10
	cmdName      = "getparty"
	userAgent    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_2) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/55.0.2883.95 Safari/537.36"
)

var (
	version              = "devel"
	contentDispositionRe *regexp.Regexp
	// Command line options
	options Options
	// flags parser
	parser *flags.Parser
)

type ActualLocation struct {
	Location          string
	SuggestedFileName string
	ContentMD5        string
	AcceptRanges      string
	StatusCode        int
	ContentLength     int64
	Parts             map[int]*Part
}

type Part struct {
	Name                 string
	Start, Stop, Written int64
	Skip                 bool
}

type Options struct {
	StateFileName string `short:"c" long:"continue" description:"resume download from last saved json state" value-name:"state.json"`
	Parts         int    `short:"p" long:"parts" default:"2" description:"number of parts"`
}

func init() {
	// https://regex101.com/r/N4AovD/3
	contentDispositionRe = regexp.MustCompile(`filename[^;\n=]*=(['"](.*?)['"]|[^;\n]*)`)
	parser = flags.NewParser(&options, flags.Default)
	parser.Name = cmdName
	parser.Usage = "[OPTIONS] url"
}

func main() {
	args, err := parser.Parse()
	if err != nil {
		if e, ok := err.(*flags.Error); ok && e.Type == flags.ErrHelp {
			os.Exit(0)
		} else {
			fmt.Println()
			parser.WriteHelp(os.Stderr)
			os.Exit(1)
		}
	}

	var wg sync.WaitGroup
	var al *ActualLocation
	var userURL string
	ctx, cancel := context.WithCancel(context.Background())
	pb := mpb.New(ctx).SetWidth(60)

	if options.StateFileName != "" {
		al, err = loadActualLocationFromJson(options.StateFileName)
		exitOnError(err)
		userURL = al.Location
		temp, err := follow(userURL, userAgent)
		exitOnError(errors.Wrapf(err, "cannot resolve %q", al.Location))
		al.Location = temp.Location
		for n, part := range al.Parts {
			if !part.Skip {
				wg.Add(1)
				go part.download(ctx, &wg, pb, al.Location, userAgent, n)
			}
		}
	} else if len(args) == 1 {
		userURL = parseURL(args[0]).String()
		al, err = follow(userURL, userAgent)
		exitOnError(errors.Wrapf(err, "cannot resolve %q", userURL))
		if al.AcceptRanges == "bytes" && al.StatusCode == http.StatusOK {
			if al.SuggestedFileName == "" {
				al.SuggestedFileName = filepath.Base(userURL)
			}
			al.calcParts(options.Parts)
			for n, part := range al.Parts {
				wg.Add(1)
				go part.download(ctx, &wg, pb, al.Location, userAgent, n)
			}
		}
	} else {
		fmt.Println("Nothing to do...")
		parser.WriteHelp(os.Stderr)
		os.Exit(1)
	}

	go onCancelSignal(cancel)
	wg.Wait()
	pb.Stop()

	var totalWritten int64
	for _, p := range al.Parts {
		totalWritten += p.Written
	}
	fmt.Fprintf(os.Stderr, "totalWritten = %+v\n", totalWritten)
	if totalWritten == al.ContentLength {
		logIfError(al.concatenateParts())
		logIfError(os.Remove(al.Parts[1].Name + ".json"))
	} else {
		logIfError(al.marshalState(userURL))
	}

}

func (al *ActualLocation) calcParts(totalParts int) {
	partSize := al.ContentLength / int64(totalParts)
	if partSize == 0 {
		partSize = al.ContentLength / 2
	}
	al.Parts = make(map[int]*Part)

	stop := al.ContentLength
	start := stop
	for i := totalParts; i > 1; i-- {
		stop = start - 1
		start = stop - partSize
		al.Parts[i] = &Part{
			Name:  fmt.Sprintf("%s.part%d", al.SuggestedFileName, i),
			Start: start,
			Stop:  stop,
		}
		// fmt.Printf("al.Parts[%d] = %+v\n", i, al.Parts[i])
	}
	al.Parts[1] = &Part{
		Name: al.SuggestedFileName,
		Stop: start - 1,
	}
	// fmt.Printf("al.Parts[%d] = %+v\n", 1, al.Parts[1])
}

func (p *Part) download(ctx context.Context, wg *sync.WaitGroup, pb *mpb.Progress, url, userAgent string, n int) {
	defer wg.Done()
	if p.Written-1 == p.Stop {
		return
	}
	name := fmt.Sprintf("part#%02d:", n)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		log.Println(name, err)
		return
	}

	start := p.Start
	if p.Written > 0 {
		start = start + p.Written
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, p.Stop))

	fmt.Fprintf(os.Stderr, "%s Range = %+v\n", name, req.Header.Get("Range"))

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		log.Printf("%s %v\n", name, err)
		return
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "resp.StatusCode = %+v\n", resp.StatusCode)

	total := p.Stop - p.Start + 1
	if resp.StatusCode == http.StatusOK {
		total = resp.ContentLength
		if n == 1 {
			p.Stop = total - 1
		} else {
			p.Skip = true
			return
		}
	} else if resp.StatusCode != 206 {
		log.Printf("%s status %d\n", name, resp.StatusCode)
		return
	}

	var dst *os.File
	if p.Written > 0 {
		dst, err = os.OpenFile(p.Name, os.O_APPEND|os.O_WRONLY, 0644)
	} else {
		dst, err = os.Create(p.Name)
	}

	if err != nil {
		log.Println(name, err)
		return
	}

	fmt.Fprintf(os.Stderr, "%s total = %+v\n", name, total)
	bar := pb.AddBar(total).
		PrependName(name, 0).
		PrependCounters(mpb.UnitBytes, 20).
		AppendETA(-6)
	bar.Incr(int(p.Written))

	// create proxy reader
	reader := bar.ProxyReader(resp.Body)
	// and copy from reader
	written, err := io.Copy(dst, reader)
	fmt.Fprintf(os.Stderr, "%s written = %+v\n", name, p.Written)
	p.Written += written
	fmt.Fprintf(os.Stderr, "%s p.Written = %+v\n", name, p.Written)

	if errc := dst.Close(); err == nil {
		err = errc
	}
	if err != nil {
		log.Println(name, err)
	}
}

func follow(userURL string, userAgent string) (*ActualLocation, error) {
	logger := log.New(os.Stdout, "[ ", log.LstdFlags)
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	next := userURL
	var al *ActualLocation
	var redirectsFollowed int
	for {
		logger.Printf("] %s\n", next)
		fmt.Printf("HTTP request sent, awaiting response... ")
		resp, err := getResp(client, next, userAgent)
		if err != nil {
			return nil, err
		}
		fmt.Println(resp.Status)
		if resp.StatusCode == http.StatusOK {
			fmt.Printf("Length: %d [%s]\n\n", resp.ContentLength, resp.Header.Get("Content-Type"))
		}

		al = &ActualLocation{
			Location:          next,
			SuggestedFileName: parseContentDisposition(resp.Header.Get("Content-Disposition")),
			AcceptRanges:      resp.Header.Get("Accept-Ranges"),
			StatusCode:        resp.StatusCode,
			ContentLength:     resp.ContentLength,
			ContentMD5:        resp.Header.Get("Content-MD5"),
		}

		if !isRedirect(resp.StatusCode) {
			break
		}

		loc, err := resp.Location()
		if err != nil {
			return nil, errors.Wrap(err, "unable to follow redirect")
		}
		redirectsFollowed++
		if redirectsFollowed > maxRedirects {
			return nil, errors.Errorf("maximum number of redirects (%d) followed", maxRedirects)
		}
		next = loc.String()
		fmt.Printf("Location: %s [following]\n", next)
	}
	return al, nil
}

func getResp(client *http.Client, url, userAgent string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return resp, nil
}

func parseContentDisposition(input string) string {
	groups := contentDispositionRe.FindAllStringSubmatch(input, -1)
	if groups == nil {
		return ""
	}
	for _, group := range groups {
		if group[2] != "" {
			return group[2]
		}
		split := strings.Split(group[1], "'")
		if len(split) == 3 && strings.ToLower(split[0]) == "utf-8" {
			unescaped, _ := url.QueryUnescape(split[2])
			return unescaped
		}
		if split[0] != `""` {
			return split[0]
		}
	}
	return ""
}

func (al *ActualLocation) concatenateParts() error {
	fpart1, err := os.OpenFile(al.Parts[1].Name, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	buf := make([]byte, 2048)
	for i := 2; i <= len(al.Parts); i++ {
		if al.Parts[i].Skip {
			continue
		}
		fparti, err := os.Open(al.Parts[i].Name)
		if err != nil {
			return err
		}
		for {
			n, err := fparti.Read(buf[0:])
			_, errw := fpart1.Write(buf[0:n])
			if errw != nil {
				return err
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
		}
		logIfError(fparti.Close())
		logIfError(os.Remove(al.Parts[i].Name))
	}
	return fpart1.Close()
}

func onCancelSignal(cancel context.CancelFunc) {
	defer cancel()
	sigs := make(chan os.Signal)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigs
	fmt.Printf("%v: canceling...\n", sig)
}

func (al *ActualLocation) marshalState(userURL string) error {
	jsonFileName := al.SuggestedFileName + ".json"
	fmt.Printf("writing state to %q\n", jsonFileName)
	al.Location = userURL // preserve user provided url
	data, err := json.Marshal(al)
	if err != nil {
		return err
	}
	dst, err := os.Create(jsonFileName)
	if err != nil {
		return err
	}
	_, err = dst.Write(data)
	if errc := dst.Close(); err == nil {
		err = errc
	}
	return err
}

func loadActualLocationFromJson(filename string) (*ActualLocation, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	al := new(ActualLocation)
	err = json.Unmarshal(data, al)
	if err != nil {
		return nil, err
	}
	return al, nil
}

func parseURL(uri string) *url.URL {
	if !strings.Contains(uri, "://") && !strings.HasPrefix(uri, "//") {
		uri = "//" + uri
	}

	url, err := url.Parse(uri)
	if err != nil {
		log.Fatalf("could not parse url %q: %v", uri, err)
	}

	if url.Scheme == "" {
		url.Scheme = "http"
		if !strings.HasSuffix(url.Host, ":80") {
			url.Scheme += "s"
		}
	}
	return url
}

func isRedirect(status int) bool {
	return status > 299 && status < 400
}

func exitOnError(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func logIfError(err error) {
	if err != nil {
		log.Println(err)
	}
}
