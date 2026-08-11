package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/russellwallace/veritix/internal/engine"
	"github.com/russellwallace/veritix/internal/store"
)

// datasetJSON is the wire shape. The store's types are not serialised
// directly, so that renaming a field in Go is not a breaking API change.
type datasetJSON struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	Uploaded  bool      `json:"uploaded"`
	CreatedAt time.Time `json:"created_at"`
}

func toDatasetJSON(d *store.Dataset) datasetJSON {
	return datasetJSON{
		ID: d.ID, Name: d.Name, Path: d.Path,
		Uploaded: d.Uploaded, CreatedAt: d.CreatedAt,
	}
}

func (s *Server) handleListDatasets(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.Datasets(r.Context())
	if err != nil {
		s.writeStoreError(w, err, "could not list datasets")
		return
	}

	out := make([]datasetJSON, 0, len(all))
	for _, d := range all {
		out = append(out, toDatasetJSON(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasets": out})
}

func (s *Server) handleGetDataset(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.Dataset(r.Context(), r.PathValue("datasetId"))
	if err != nil {
		s.writeStoreError(w, err, "could not read the dataset")
		return
	}
	writeJSON(w, http.StatusOK, toDatasetJSON(d))
}

type createDatasetRequest struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// handleCreateDataset registers a dataset either by server path or by upload,
// deciding between them on the content type.
func (s *Server) handleCreateDataset(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		s.createDatasetFromUpload(w, r)
		return
	}

	var req createDatasetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%s", err)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "path is required: give a file or directory on this server")
		return
	}

	// Registering a path means auditing data that is already on the server, so
	// it is checked for existence but not confined to a root: choosing what
	// this server may read is the operator's decision, and Veritix is
	// single-tenant by design. The bearer token is what limits who may make it.
	abs, err := filepath.Abs(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not resolve %q: %s", req.Path, err)
		return
	}
	if _, err := os.Stat(abs); err != nil {
		writeError(w, http.StatusBadRequest, "cannot read %s: %s", abs, err)
		return
	}

	name := req.Name
	if name == "" {
		name = filepath.Base(abs)
	}

	d, err := s.store.CreateDataset(r.Context(), name, abs, false)
	if err != nil {
		s.writeStoreError(w, err, "could not register the dataset")
		return
	}
	writeJSON(w, http.StatusCreated, toDatasetJSON(d))
}

// createDatasetFromUpload accepts a folder or workbook from the browser.
//
// This is the main way a business user supplies data: they are on a Windows
// desktop and the files are on it, not on the server.
func (s *Server) createDatasetFromUpload(w http.ResponseWriter, r *http.Request) {
	limit := s.cfg.Server.MaxUploadBytes
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	// 32 MiB in memory; the rest of a large upload spills to temporary files.
	// The total is already bounded by the MaxBytesReader above, which is what
	// G120 is looking for and cannot see from here.
	if err := r.ParseMultipartForm(32 << 20); err != nil { //nolint:gosec // bounded by MaxBytesReader above
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				"the upload exceeds the %d byte limit; raise server.max_upload_bytes to accept it", limit)
			return
		}
		writeError(w, http.StatusBadRequest, "could not read the upload: %s", err)
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "no files in the upload: send them in the 'files' field")
		return
	}

	name := r.FormValue("name")
	if name == "" {
		name = "upload"
		// A single workbook is its own dataset and deserves its own name; a
		// folder of exports is one dataset and the caller should have named it.
		if len(files) == 1 {
			name = strings.TrimSuffix(files[0].Filename, filepath.Ext(files[0].Filename))
		}
	}

	dir, err := s.uploadDir(name)
	if err != nil {
		s.log.Error("could not create the upload directory", "error", err)
		writeError(w, http.StatusInternalServerError, "could not store the upload")
		return
	}

	for _, fh := range files {
		if err := saveUpload(dir, fh); err != nil {
			_ = os.RemoveAll(dir)
			writeError(w, http.StatusBadRequest, "%s", err)
			return
		}
	}

	d, err := s.store.CreateDataset(r.Context(), name, dir, true)
	if err != nil {
		_ = os.RemoveAll(dir)
		s.writeStoreError(w, err, "could not register the upload")
		return
	}
	writeJSON(w, http.StatusCreated, toDatasetJSON(d))
}

// uploadDir makes a fresh directory under the data directory for one upload.
// The name is only a readable prefix; the suffix is what makes it unique, so
// two uploads called "exports" cannot land on top of each other.
//
// Mkdir rather than MkdirAll, with a random suffix: Mkdir creates the
// directory or fails, so the uniqueness is structural rather than hoped for.
// The previous version appended the first eight characters of a UUIDv7 and
// described them as random — they are the high bits of the millisecond
// timestamp and do not change for about a minute, so two uploads of the same
// folder within a minute shared a directory. MkdirAll returned the existing one
// without complaint, and the upload failed later on the first file that was
// already there.
func (s *Server) uploadDir(name string) (string, error) {
	root := filepath.Join(s.cfg.Server.DataDir, "datasets")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", err
	}

	// SafeName reduces the caller's name to letters, digits, and underscores,
	// so the only part of this path that is not a constant is a sanitised name
	// followed by bytes from crypto/rand.
	safe := engine.SafeName(name)
	for attempt := 0; attempt < 5; attempt++ {
		var suffix [6]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("could not name the upload directory: %w", err)
		}

		dir := filepath.Join(root, safe+"-"+hex.EncodeToString(suffix[:]))
		err := os.Mkdir(dir, 0o750) //nolint:gosec // name is sanitised, suffix is random, root is the data dir
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not find an unused directory for the upload")
}

// saveUpload writes one uploaded file into dir.
//
// A multipart filename is attacker-controlled — "../../etc/cron.d/x" is a
// legal value — so it is reduced to a base name and then re-checked, and the
// result is never joined onto anything the request chose. Browsers uploading a
// folder send relative paths in the filename, which is why the base name is
// taken rather than the whole thing rejected.
func saveUpload(dir string, fh *multipart.FileHeader) error {
	base := filepath.Base(filepath.FromSlash(fh.Filename))
	if base == "." || base == ".." || base == string(filepath.Separator) || base == "" {
		return fmt.Errorf("refusing an upload named %q", fh.Filename)
	}

	dst := filepath.Join(dir, base)
	if filepath.Dir(dst) != filepath.Clean(dir) {
		return fmt.Errorf("refusing an upload named %q", fh.Filename)
	}

	src, err := fh.Open()
	if err != nil {
		return fmt.Errorf("could not read %s: %w", base, err)
	}
	defer src.Close() //nolint:errcheck // read side; the write below is what can fail meaningfully

	// O_EXCL: two files whose names differ only by directory collapse to the
	// same base name, and silently overwriting one with the other would audit
	// a dataset the user did not send.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640) //nolint:gosec // dst is dir + a base name, checked above
	if err != nil {
		return fmt.Errorf("could not store %s: %w", base, err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		return fmt.Errorf("could not store %s: %w", base, err)
	}
	return out.Close()
}

func (s *Server) handleDeleteDataset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("datasetId")

	d, err := s.store.Dataset(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err, "could not read the dataset")
		return
	}

	// Collect the runs before the cascade removes them: their DuckDB files are
	// on disk and the database is the only record of where.
	runs, err := s.store.Runs(r.Context(), id, allRuns)
	if err != nil {
		s.writeStoreError(w, err, "could not list the dataset's runs")
		return
	}

	if err := s.store.DeleteDataset(r.Context(), id); err != nil {
		s.writeStoreError(w, err, "could not delete the dataset")
		return
	}

	for _, run := range runs {
		s.runs.cancel(run.ID)
		s.removeRunFiles(run)
	}
	// Only bytes Veritix wrote itself. A path an operator registered in place
	// is theirs, and forgetting a dataset must not delete the customer's data.
	if d.Uploaded {
		// Uploaded means the path came from uploadDir, under the data
		// directory. A registered path never reaches here.
		if err := os.RemoveAll(d.Path); err != nil { //nolint:gosec // only paths this server wrote
			s.log.Warn("could not remove uploaded files", "path", d.Path, "error", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// removeRunFiles deletes the DuckDB database a run left behind, and only if it
// sits where the server puts them.
func (s *Server) removeRunFiles(run *store.Run) {
	if run.DatabasePath == "" {
		return
	}
	dir := filepath.Dir(run.DatabasePath)
	if !strings.HasPrefix(dir, filepath.Join(s.cfg.Server.DataDir, "runs")+string(filepath.Separator)) {
		return
	}
	if err := os.RemoveAll(dir); err != nil { //nolint:gosec // confined to DataDir/runs by the check above
		s.log.Warn("could not remove run database", "run", run.ID, "path", dir, "error", err)
	}
}
