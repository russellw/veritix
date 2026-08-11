// Package source finds the files that make up a dataset and works out how to
// read each one.
//
// Veritix treats a directory as a single dataset rather than a pile of
// unrelated files. Business data arrives as a folder of exports that reference
// each other, and most real integrity problems live in the relationships
// between those files rather than inside any one of them.
package source

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/russellw/veritix/internal/engine"
)

// Kind identifies how a file is read.
type Kind string

const (
	// KindCSV is any delimited text file, whatever its delimiter: the
	// extension is a hint and the sniffer has the final say.
	KindCSV Kind = "csv"
	// KindExcel is a workbook, which may hold several tables.
	KindExcel Kind = "excel"
)

// File is one physical input file.
type File struct {
	// Path is the absolute path on disk.
	Path string
	// Rel is the path relative to the dataset root, used for display.
	Rel string
	// Kind is how the file will be read.
	Kind Kind
	// Size is the file size in bytes.
	Size int64
}

// TableRef names one logical table within the dataset. A CSV file yields
// exactly one; an Excel workbook yields one per worksheet.
type TableRef struct {
	// Name is a unique, SQL-friendly identifier within the dataset.
	Name string
	// Display is the human-readable origin, e.g. "sales.xlsx#Q1".
	Display string
	// File is the file this table is read from.
	File File
	// Sheet is the worksheet name, empty for CSV.
	Sheet string
}

// Dataset is the set of files and tables discovered under one or more paths.
type Dataset struct {
	// Root is the common directory the dataset was discovered under.
	Root string
	// Files are the inputs Veritix understands.
	Files []File
	// Tables are the logical tables those files contain.
	Tables []TableRef
	// Skipped records files that were passed over, and why. These are worth
	// surfacing: a spreadsheet in an unreadable format is itself a finding a
	// user needs to know about, not something to silently ignore.
	Skipped []SkippedFile
}

// SkippedFile is an input Veritix declined to read.
type SkippedFile struct {
	Path   string
	Rel    string
	Reason string
}

// ErrNoTables reports that nothing readable was found.
var ErrNoTables = errors.New("source: no readable data files found")

// csvExtensions maps a file extension to the delimiter it conventionally
// implies. An empty value means "let the sniffer decide".
var csvExtensions = map[string]string{
	".csv": "",
	".tsv": "\t",
	".tab": "\t",
	".psv": "|",
	".txt": "",
}

var excelExtensions = map[string]bool{
	".xlsx": true,
	".xlsm": true,
	".xltx": true,
	".xltm": true,
}

// Discover walks the given paths and builds a dataset description. Directories
// are walked recursively; individual files are taken as given.
func Discover(paths []string) (*Dataset, error) {
	ds := &Dataset{}
	seen := make(map[string]bool)

	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("source: resolving %s: %w", p, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("source: %w", err)
		}

		if !info.IsDir() {
			ds.add(abs, info, seen)
			continue
		}
		walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// An unreadable subdirectory should not abort the whole audit;
				// record it and carry on.
				ds.Skipped = append(ds.Skipped, SkippedFile{Path: path, Reason: err.Error()})
				return nil //nolint:nilerr // deliberate: degrade rather than abort
			}
			if d.IsDir() {
				if shouldSkipDir(d.Name()) && path != abs {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil {
				ds.Skipped = append(ds.Skipped, SkippedFile{Path: path, Reason: err.Error()})
				return nil //nolint:nilerr // deliberate: degrade rather than abort
			}
			ds.add(path, info, seen)
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("source: walking %s: %w", abs, walkErr)
		}
	}

	ds.Root = commonRoot(paths)
	ds.finalize()

	if len(ds.Files) == 0 {
		return ds, ErrNoTables
	}
	return ds, nil
}

func (ds *Dataset) add(path string, info fs.FileInfo, seen map[string]bool) {
	if seen[path] {
		return
	}
	seen[path] = true

	name := filepath.Base(path)
	ext := strings.ToLower(filepath.Ext(name))

	switch {
	case strings.HasPrefix(name, "."):
		// Hidden files are editor state and OS metadata far more often than
		// they are data.
		return
	case strings.HasPrefix(name, "~$"):
		// Excel's lock file for an open workbook; it is not a workbook.
		ds.Skipped = append(ds.Skipped, SkippedFile{
			Path: path, Reason: "Excel lock file for a workbook that is currently open",
		})
		return
	case ext == ".xls":
		ds.Skipped = append(ds.Skipped, SkippedFile{
			Path: path,
			Reason: "legacy .xls format is not supported; re-save as .xlsx " +
				"(the binary format predates the OOXML standard)",
		})
		return
	case info.Size() == 0:
		ds.Skipped = append(ds.Skipped, SkippedFile{Path: path, Reason: "file is empty"})
		return
	}

	f := File{Path: path, Size: info.Size()}
	switch {
	case excelExtensions[ext]:
		f.Kind = KindExcel
	default:
		if _, ok := csvExtensions[ext]; !ok {
			return // not a data file we recognize; not worth reporting
		}
		f.Kind = KindCSV
	}
	ds.Files = append(ds.Files, f)
}

// finalize fills in relative paths and sorts everything into a stable order so
// that two runs over the same directory produce identical reports.
func (ds *Dataset) finalize() {
	for i := range ds.Files {
		ds.Files[i].Rel = relTo(ds.Root, ds.Files[i].Path)
	}
	for i := range ds.Skipped {
		ds.Skipped[i].Rel = relTo(ds.Root, ds.Skipped[i].Path)
	}
	sort.Slice(ds.Files, func(i, j int) bool { return ds.Files[i].Rel < ds.Files[j].Rel })
	sort.Slice(ds.Skipped, func(i, j int) bool { return ds.Skipped[i].Rel < ds.Skipped[j].Rel })
}

func relTo(root, path string) string {
	if root == "" {
		return path
	}
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", ".svn", ".hg", "node_modules", "__pycache__", ".venv", "venv":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// commonRoot finds the deepest directory containing all the given paths, so
// that reports can show short relative names.
func commonRoot(paths []string) string {
	var dirs []string
	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			abs = filepath.Dir(abs)
		}
		dirs = append(dirs, abs)
	}
	if len(dirs) == 0 {
		return ""
	}

	common := strings.Split(filepath.Clean(dirs[0]), string(filepath.Separator))
	for _, d := range dirs[1:] {
		parts := strings.Split(filepath.Clean(d), string(filepath.Separator))
		n := min(len(common), len(parts))
		i := 0
		for i < n && common[i] == parts[i] {
			i++
		}
		common = common[:i]
	}
	root := strings.Join(common, string(filepath.Separator))
	if root == "" {
		return string(filepath.Separator)
	}
	return root
}

// AssignNames gives every table a unique, SQL-friendly name. Collisions are
// resolved by adding progressively more of the path, so that
// `2024/orders.csv` and `2025/orders.csv` become `orders_csv` and
// `t_2025_orders_csv` rather than `orders_csv` and `orders_csv_2`.
func AssignNames(tables []TableRef) {
	used := make(map[string]int, len(tables))

	for i := range tables {
		base := engine.SafeName(tables[i].Display)
		name := base
		if n, taken := used[name]; taken {
			// Fall back to a numeric suffix only once the qualified name is
			// still ambiguous.
			qualified := engine.SafeName(tables[i].File.Rel)
			if tables[i].Sheet != "" {
				qualified = engine.SafeName(tables[i].File.Rel + " " + tables[i].Sheet)
			}
			if _, alsoTaken := used[qualified]; !alsoTaken {
				name = qualified
			} else {
				used[base] = n + 1
				name = fmt.Sprintf("%s_%d", base, n+1)
			}
		}
		used[name] = 1
		tables[i].Name = name
	}
}
