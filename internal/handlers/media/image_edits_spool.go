package media

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"os"
	"strings"
)

// maxTextFieldBytes bounds one non-file multipart field; a prompt or model id
// is never larger, so a bigger "field" means a malformed or abusive body.
const maxTextFieldBytes = 64 << 10

// spooledPart is one part of the caller's upload, kept in original order. File
// parts spill to a temp file so the upload is never held in one []byte; text
// parts stay in memory (they are tiny by maxTextFieldBytes).
type spooledPart struct {
	field  string
	header textproto.MIMEHeader
	isFile bool
	value  string // text parts
	path   string // temp file, empty for text parts
}

// spoolMultipartParts reads the caller's stream once. Text fields are kept in
// memory, file parts are written to temp files, and the running byte total is
// capped: the first write past imageEditsMaxUpload fails the whole spool.
func spoolMultipartParts(mr *multipart.Reader) ([]spooledPart, error) {
	total := int64(0)
	var parts []spooledPart
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return parts, err
		}
		if p.FileName() != "" {
			tmp, err := os.CreateTemp("", "9router-edit-*")
			if err != nil {
				return parts, err
			}
			sp := spooledPart{field: p.FormName(), header: p.Header, isFile: true, path: tmp.Name()}
			cw := cappedWriter{w: tmp, total: &total}
			_, copyErr := io.Copy(&cw, p)
			tmp.Close()
			if copyErr != nil {
				return parts, copyErr
			}
			parts = append(parts, sp)
			continue
		}
		val, err := io.ReadAll(io.LimitReader(p, maxTextFieldBytes+1))
		if err != nil {
			return parts, err
		}
		if len(val) > maxTextFieldBytes {
			return parts, fmt.Errorf("field %q exceeds %d bytes", p.FormName(), maxTextFieldBytes)
		}
		total += int64(len(val))
		if total > imageEditsMaxUpload {
			return parts, errEditsTooLarge
		}
		parts = append(parts, spooledPart{field: p.FormName(), header: p.Header, value: string(val)})
	}
	return parts, nil
}

// cappedWriter counts bytes and fails once the running total passes the cap,
// so the spool aborts mid-copy instead of draining an oversized part fully.
type cappedWriter struct {
	w     io.Writer
	total *int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if *c.total+int64(len(p)) > imageEditsMaxUpload {
		return 0, errEditsTooLarge
	}
	n, err := c.w.Write(p)
	*c.total += int64(n)
	return n, err
}

// writeSpooledParts replays the spooled parts into mw in original order. Only
// the model field is rewritten (to the stripped upstream id); everything else
// — including binary image bytes — is copied verbatim.
func writeSpooledParts(mw *multipart.Writer, parts []spooledPart, model string) error {
	for _, p := range parts {
		fw, err := mw.CreatePart(p.header)
		if err != nil {
			return err
		}
		if !p.isFile {
			value := p.value
			if p.field == "model" && model != "" {
				value = model
			}
			if _, err := fw.Write([]byte(value)); err != nil {
				return err
			}
			continue
		}
		f, err := os.Open(p.path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(fw, f)
		f.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	return mw.Close()
}

func cleanupSpooled(parts []spooledPart) {
	for _, p := range parts {
		if p.path != "" {
			os.Remove(p.path)
		}
	}
}

func hasImageFile(parts []spooledPart) bool {
	for _, p := range parts {
		if p.isFile && (p.field == "image" || p.field == "image[]" || p.field == "mask") {
			return true
		}
	}
	return false
}

func fieldValue(parts []spooledPart, name string) string {
	for _, p := range parts {
		if p.field == name {
			return strings.TrimSpace(p.value)
		}
	}
	return ""
}
