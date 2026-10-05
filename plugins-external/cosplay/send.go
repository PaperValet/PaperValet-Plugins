// Downloading images and sending them as a Telegram photo album.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/TiaraBasori/PaperValet/pkg/plugin"
)

const (
	downloadTimeout = 120 * time.Second
	maxImageBytes   = 50 << 20
	maxRetries      = 3
	dlConcurrency   = 3
	albumMax        = 10 // Telegram album size cap
)

// httpErr wraps a non-200 status so retry logic can inspect it.
type httpErr struct {
	status int
	url    string
}

func (e httpErr) Error() string {
	return fmt.Sprintf("HTTP %d for %s", e.status, e.url)
}

// retryableDownload decides whether a failed download is worth retrying,
// like the reference's classifyError (5xx/429/timeouts retry, 4xx not).
func retryableDownload(err error) bool {
	var he httpErr
	if errors.As(err, &he) {
		return he.status >= 500 || he.status == 429
	}
	return !errors.Is(err, context.Canceled)
}

// download fetches the images concurrently (dlConcurrency at a time, like
// the reference's p-limit) into temp files and returns the paths that
// succeeded; if none did, the first error is returned.
func (p *CosplayPlugin) download(ctx context.Context, urls []string) ([]string, error) {
	paths := make([]string, len(urls))
	errs := make([]error, len(urls))
	sem := make(chan struct{}, dlConcurrency)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			paths[i], errs[i] = p.downloadOne(ctx, u)
		}(i, u)
	}
	wg.Wait()

	var files []string
	var firstErr error
	for i := range urls {
		if errs[i] != nil {
			if firstErr == nil && !errors.Is(errs[i], context.Canceled) {
				firstErr = errs[i]
			}
			continue
		}
		files = append(files, paths[i])
	}
	if len(files) == 0 {
		if firstErr == nil {
			firstErr = errors.New("no images downloaded")
		}
		return nil, firstErr
	}
	return files, nil
}

// downloadOne streams one image into a temp file with up to maxRetries
// attempts and exponential backoff, like the reference's withRetry.
func (p *CosplayPlugin) downloadOne(ctx context.Context, rawURL string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if attempt > 0 {
			p.sleep(ctx, time.Duration(1<<(attempt-1))*time.Second)
		}
		path, err := p.downloadOnce(ctx, rawURL)
		if err == nil {
			return path, nil
		}
		if !retryableDownload(err) {
			return "", err
		}
		lastErr = err
	}
	return "", lastErr
}

// downloadOnce streams rawURL into a temp file under the system temp dir
// (the reference's cleanup deletes these after sending).
func (p *CosplayPlugin) downloadOnce(ctx context.Context, rawURL string) (string, error) {
	dctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", siteUA)
	req.Header.Set("Accept", "image/webp,image/apng,image/*,*/*;q=0.8")
	if strings.Contains(rawURL, domainOrHost(baseURL)) {
		req.Header.Set("Referer", baseURL)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", httpErr{status: resp.StatusCode, url: rawURL}
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" &&
		!strings.HasPrefix(ct, "image/") && !strings.HasPrefix(ct, "application/octet-stream") {
		return "", fmt.Errorf("unexpected content type %s", ct)
	}
	f, err := os.CreateTemp("", "cos_*.img")
	if err != nil {
		return "", err
	}
	path := f.Name()
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxImageBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		os.Remove(path)
		return "", err
	case n == 0:
		os.Remove(path)
		return "", errors.New("empty image")
	case n > maxImageBytes:
		os.Remove(path)
		return "", fmt.Errorf("image over %dMB", maxImageBytes>>20)
	}
	// Give the file its real extension (used when sending as a document).
	if ext := strings.ToLower(filepath.Ext(strings.Split(rawURL, "?")[0])); ext != "" {
		if dst := strings.TrimSuffix(path, ".img") + ext; dst != path {
			if err := os.Rename(path, dst); err == nil {
				path = dst
			}
		}
	}
	return path, nil
}

// cleanup removes downloaded temp files, best effort (reference cleanup).
func (p *CosplayPlugin) cleanup(files []string) {
	for _, f := range files {
		os.Remove(f)
	}
}

// ---------------------------------------------------------------- sending

// sendAlbum sends the files as a (spoilered) photo album in batches of 10;
// only the first batch carries the caption, like the reference.
func (p *CosplayPlugin) sendAlbum(ctx *plugin.CommandContext, files []string, caption string, spoiler bool) error {
	for len(files) > 0 {
		batch := files
		if len(batch) > albumMax {
			batch = batch[:albumMax]
		}
		if err := p.sendBatch(ctx, batch, caption, spoiler); err != nil {
			return err
		}
		files = files[albumMax:]
		caption = ""
	}
	return nil
}

// sendBatch sends one album; when the album fails it falls back to sending
// the photos one by one, like the reference's sendImageAlbum fallback.
func (p *CosplayPlugin) sendBatch(ctx *plugin.CommandContext, files []string, caption string, spoiler bool) error {
	if len(files) == 1 {
		return p.sendOnePhoto(ctx, files[0], caption, spoiler)
	}
	if err := p.sendMultiMedia(ctx, files, caption, spoiler); err != nil {
		if ctx.Logger != nil {
			ctx.Logger.Warn("cosplay: album failed, falling back to single photos", "err", err)
		}
		var firstErr error
		for _, f := range files {
			if err := p.sendOnePhoto(ctx, f, "", spoiler); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}
	return nil
}

// sendOnePhoto uploads one file as a photo with the spoiler flag, like the
// reference's sendSingleImage.
func (p *CosplayPlugin) sendOnePhoto(ctx *plugin.CommandContext, path, caption string, spoiler bool) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	file, err := uploader.NewUploader(ctx.API).FromPath(ctx.Context(), path)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	media := &tg.InputMediaUploadedPhoto{File: file}
	if spoiler {
		media.SetSpoiler(true)
	}
	_, err = ctx.API.MessagesSendMedia(ctx.Context(), &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    media,
		Message:  caption,
		RandomID: rand.Int64(),
	})
	if err != nil && isPhotoReject(err) {
		// Telegram can reject a photo (dimensions, format); retry as a
		// document, which accepts anything.
		if ctx.Logger != nil {
			ctx.Logger.Warn("cosplay: photo rejected, retrying as document", "err", err)
		}
		return p.sendDocument(ctx, path, caption, spoiler)
	}
	return err
}

// sendDocument uploads one file as a document (spoiler still supported).
func (p *CosplayPlugin) sendDocument(ctx *plugin.CommandContext, path, caption string, spoiler bool) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	file, err := uploader.NewUploader(ctx.API).FromPath(ctx.Context(), path)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	mt := mime.TypeByExtension(filepath.Ext(path))
	if mt == "" || !strings.HasPrefix(mt, "image/") {
		mt = "image/jpeg"
	}
	doc := &tg.InputMediaUploadedDocument{
		File:     file,
		MimeType: mt,
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: filepath.Base(path)},
		},
	}
	if spoiler {
		doc.SetSpoiler(true)
	}
	_, err = ctx.API.MessagesSendMedia(ctx.Context(), &tg.MessagesSendMediaRequest{
		Peer:     peer,
		Media:    doc,
		Message:  caption,
		RandomID: rand.Int64(),
	})
	return err
}

// sendMultiMedia sends an album via messages.sendMultiMedia with the
// caption on the first item, like the reference's sendMediaGroup.
func (p *CosplayPlugin) sendMultiMedia(ctx *plugin.CommandContext, files []string, caption string, spoiler bool) error {
	peer, err := ctx.ResolvePeer()
	if err != nil {
		return err
	}
	media := make([]tg.InputSingleMedia, 0, len(files))
	for i, f := range files {
		file, err := uploader.NewUploader(ctx.API).FromPath(ctx.Context(), f)
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		m := &tg.InputMediaUploadedPhoto{File: file}
		if spoiler {
			m.SetSpoiler(true)
		}
		msg := ""
		if i == 0 {
			msg = caption
		}
		media = append(media, tg.InputSingleMedia{
			Media:    m,
			RandomID: rand.Int64(),
			Message:  msg,
		})
	}
	_, err = ctx.API.MessagesSendMultiMedia(ctx.Context(), &tg.MessagesSendMultiMediaRequest{
		Peer:       peer,
		MultiMedia: media,
	})
	return err
}

// isPhotoReject reports whether Telegram rejected the photo payload
// itself, so sending it as a document may still work.
func isPhotoReject(err error) bool {
	var gerr *tgerr.Error
	if errors.As(err, &gerr) {
		switch gerr.Type {
		case "PHOTO_INVALID_DIMENSIONS", "PHOTO_INVALID_EXTENT",
			"PHOTO_SAVE_FILE_INVALID", "PHOTO_INVALID", "FILE_PARTS_INVALID":
			return true
		}
	}
	return false
}
