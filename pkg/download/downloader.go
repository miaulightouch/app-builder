package download

import (
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	"github.com/alecthomas/kingpin"
	"github.com/apex/log"
	"github.com/develar/app-builder/pkg/util"
	"github.com/develar/errors"
	"github.com/develar/go-fs-util"
	"github.com/dustin/go-humanize"
)

//noinspection SpellCheckingInspection
const (
	maxRedirects = 10
	minPartSize  = 5 * 1024 * 1024
)

func getUserAgent() string {
	//noinspection SpellCheckingInspection
	return util.GetEnvOrDefault("DOWNLOADER_USER_AGENT", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_14_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/74.0.3729.169 Safari/537.36")
}

func getMaxPartCount() int {
	const maxPartCount = 8
	result := runtime.NumCPU() * 2
	if result > maxPartCount {
		return maxPartCount
	} else {
		return result
	}
}

func ConfigureCommand(app *kingpin.Application) {
	command := app.Command("download", "Download file.")
	fileUrl := command.Flag("url", "The URL.").Short('u').Required().String()
	output := command.Flag("output", "The output file.").Short('o').Required().String()
	sha512 := command.Flag("sha512", "The expected sha512 of file.").String()

	command.Action(func(context *kingpin.ParseContext) error {
		return NewDownloader().Download(*fileUrl, *output, *sha512)
	})
}

type Downloader struct {
	client    *http.Client
	Transport *http.Transport
}

func NewDownloader() *Downloader {
	return NewDownloaderWithTransport(&http.Transport{
		Proxy:               util.ProxyFromEnvironmentAndNpm,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     30 * time.Second,
	})
}

func NewDownloaderWithTransport(transport *http.Transport) *Downloader {
	return &Downloader{
		Transport: transport,
		client: &http.Client{
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: transport,
		},
	}
}

func (t *Downloader) Download(url string, output string, sha512 string) error {
	start := time.Now()

	actualLocation, err := t.follow(url, getUserAgent(), output)
	if err != nil {
		return errors.WithStack(err)
	}

	err = t.DownloadResolved(actualLocation, sha512, url)
	if err != nil {
		return errors.WithStack(err)
	}

	log.WithFields(&log.Fields{
		"url":      url,
		"duration": fmt.Sprintf("%v", time.Since(start).Round(time.Millisecond)),
	}).Info("downloaded")

	return err
}

func (t *Downloader) DownloadResolved(location *ActualLocation, sha512 string, urlToLog string) error {
	err := fsutil.EnsureDir(filepath.Dir(location.OutFileName))
	if err != nil {
		return errors.WithStack(err)
	}

	downloadContext, cancel := util.CreateContext()

	location.computeParts(minPartSize)
	log.WithFields(&log.Fields{
		"url":   urlToLog,
		"size":  humanize.Bytes(uint64(location.ContentLength)),
		"parts": len(location.Parts),
	}).Info("downloading")
	err = util.MapAsyncConcurrency(len(location.Parts), getMaxPartCount(), func(index int) (func() error, error) {
		part := location.Parts[index]
		return func() error {
			err := part.download(downloadContext, location.Url, index, t.client)
			if err != nil {
				part.isFail = true
				log.WithFields(log.Fields{
					"id":    index,
					"error": err,
				}).Debug("part download error")
			}
			return err
		}, nil
	})

	if err != nil {
		return errors.WithStack(err)
	}

	for _, part := range location.Parts {
		if part.isFail {
			cancel()
			break
		}
	}

	location.deleteUnnecessaryParts()
	err = location.concatenateParts(sha512)
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (t *Downloader) follow(initialUrl, userAgent, outFileName string) (*ActualLocation, error) {
	currentUrl := initialUrl
	redirectsFollowed := 0
	for {
		if currentUrl != initialUrl {
			log.WithFields(log.Fields{
				"initialUrl": initialUrl,
				"currentUrl": currentUrl,
			}).Debug("computing effective URL")
		}

		// should use GET instead of HEAD because ContentLength maybe omitted for HEAD requests
		// https://stackoverflow.com/questions/3854842/content-length-header-with-head-requests
		request, err := http.NewRequest(http.MethodGet, currentUrl, nil)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		request.Header.Set("User-Agent", userAgent)
		actualLocation, err := func() (*ActualLocation, error) {
			response, err := t.client.Do(request)
			if response != nil {
				util.Close(response.Body)
			}

			if err != nil {
				return nil, errors.WithStack(err)
			}

			if isRedirect(response.StatusCode) {
				loc, err := response.Location()
				if err != nil {
					return nil, errors.WithStack(err)
				}

				currentUrl = loc.String()
				return nil, nil
			} else if response.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("cannot resolve %s: status code %d", initialUrl, response.StatusCode)
			}

			actualLocation := NewResolvedLocation(currentUrl, response.ContentLength, outFileName, response.Header.Get("Accept-Ranges") != "")
			var length string
			if response.ContentLength < 0 {
				length = "unknown"
			} else {
				length = fmt.Sprintf("%d", response.ContentLength)
			}

			log.WithFields(log.Fields{
				"length":       length,
				"content-type": response.Header.Get("Content-Type"),
				"url":          initialUrl,
			}).Debug("downloading")

			if !actualLocation.isAcceptRanges {
				log.Warn("server doesn't support ranges")
			}
			return &actualLocation, nil
		}()

		if err != nil {
			return nil, errors.WithStack(err)
		}

		if actualLocation != nil {
			return actualLocation, nil
		}

		redirectsFollowed++
		if redirectsFollowed > maxRedirects {
			return nil, errors.Errorf("maximum number of redirects (%d) followed", maxRedirects)
		}
	}
}

func isRedirect(status int) bool {
	return status > 299 && status < 400
}
