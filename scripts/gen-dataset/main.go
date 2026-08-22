// Command gen-dataset writes a synthetic dataset big enough to be worth
// measuring.
//
// Both committed fixtures total seventy-three rows. Everything built for size
// — the engine's memory limit and query timeout, DuckDB's spill to
// temp_directory, the profiler's fan-out across columns, the anti-joins in
// checks/relate.go, the report — has therefore never run against data that
// would reach it. This writes a dataset that does, with defects planted at
// known counts and a manifest describing them, so `veritix eval` scores the
// same run that is being timed: a scale test that only measures seconds
// cannot tell a fast auditor from one that quietly stopped looking.
//
// It is a generator rather than a committed fixture because a two-gigabyte
// fixture is not reviewable and does not belong in git history. The seed makes
// it reproducible instead: the same seed and scale write the same bytes, so a
// measurement can be repeated on another machine.
//
//	go run ./scripts/gen-dataset -out /var/tmp/big -scale 1
//	./bin/veritix eval /var/tmp/big
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

func main() {
	log.SetFlags(0)

	out := flag.String("out", "", "directory to write the dataset into (required)")
	scale := flag.Float64("scale", 1, "multiply every row count by this")
	seed := flag.Uint64("seed", 1, "seed for the generator, so a run can be repeated")
	flag.Parse()

	if *out == "" {
		log.Fatal("gen-dataset: -out is required")
	}
	if *scale <= 0 {
		log.Fatal("gen-dataset: -scale must be positive")
	}
	if err := run(*out, *scale, *seed); err != nil {
		log.Fatalf("gen-dataset: %v", err)
	}
}

// Row counts at scale 1. Orders is ten times customers because that is the
// shape of the thing being tested: the expensive query in an audit of a real
// dataset is the anti-join from the big child table to the small parent.
const (
	customerRows = 2_000_000
	orderRows    = 20_000_000
	productRows  = 50_000
	metricRows   = 200_000
	metricCols   = 200
	regionRows   = 24
)

func run(dir string, scale float64, seed uint64) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	n := func(base int) int {
		rows := int(float64(base) * scale)
		if rows < 1 {
			rows = 1
		}
		return rows
	}

	var (
		customers, orders, products, metrics counts
		group                                errgroup.Group
	)
	started := time.Now()

	group.Go(func() (err error) { return writeRegions(dir) })
	group.Go(func() (err error) {
		customers, err = writeCustomers(dir, n(customerRows), rand.NewPCG(seed, 1))
		return err
	})
	group.Go(func() (err error) {
		orders, err = writeOrders(dir, n(orderRows), n(customerRows), n(productRows), rand.NewPCG(seed, 2))
		return err
	})
	group.Go(func() (err error) {
		products, err = writeProducts(dir, n(productRows), rand.NewPCG(seed, 3))
		return err
	})
	group.Go(func() (err error) {
		metrics, err = writeMetrics(dir, n(metricRows), rand.NewPCG(seed, 4))
		return err
	})
	if err := group.Wait(); err != nil {
		return err
	}

	if err := writeManifest(dir, customers, orders); err != nil {
		return err
	}

	total, err := dirBytes(dir)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s in %s: %d customers, %d orders, %d products, %d metric rows x %d columns\n",
		humanBytes(total), time.Since(started).Round(time.Second),
		customers.rows, orders.rows, products.rows, metrics.rows, metricCols)
	return nil
}

// counts is what a table generator planted, so the manifest states the figure
// the engine will measure rather than the rate the generator was aiming for.
// A target whose count is a nice round number is a target that was estimated.
type counts struct {
	rows int

	duplicateRows   int
	duplicateKeys   int
	paddedNames     int
	altDateFormat   int
	notADate        int
	placeholders    int
	numericSentinel int
	lowercaseStatus int
	orphanRegions   int
	orphanParents   int
	implausible     int
	future          int
	negative        int
	beforeOrder     int
	raggedRows      int
}

// writer is a table being written: a buffered file plus the line under
// construction, reused so that twenty million rows do not allocate twenty
// million strings.
type writer struct {
	f   *os.File
	buf *bufio.Writer
	row []byte
}

func newWriter(path string) (*writer, error) {
	f, err := os.Create(path) //nolint:gosec // path is a generator flag, not input
	if err != nil {
		return nil, err
	}
	return &writer{f: f, buf: bufio.NewWriterSize(f, 1<<22), row: make([]byte, 0, 512)}, nil
}

func (w *writer) header(cols ...string) { w.line(cols...) }

// field appends one already-formatted value to the row under construction.
func (w *writer) field(v string) {
	if len(w.row) > 0 {
		w.row = append(w.row, ',')
	}
	w.row = append(w.row, v...)
}

func (w *writer) fieldInt(v int) {
	if len(w.row) > 0 {
		w.row = append(w.row, ',')
	}
	w.row = strconv.AppendInt(w.row, int64(v), 10)
}

func (w *writer) end() error {
	w.row = append(w.row, '\n')
	_, err := w.buf.Write(w.row)
	w.row = w.row[:0]
	return err
}

func (w *writer) line(fields ...string) {
	for _, f := range fields {
		w.field(f)
	}
	_ = w.end()
}

func (w *writer) Close() error {
	if err := w.buf.Flush(); err != nil {
		_ = w.f.Close()
		return err
	}
	return w.f.Close()
}

func writeRegions(dir string) error {
	w, err := newWriter(filepath.Join(dir, "regions.csv"))
	if err != nil {
		return err
	}
	w.header("region_code", "region_name")
	for i := 1; i <= regionRows; i++ {
		w.line(regionCode(i), fmt.Sprintf("Region %02d", i))
	}
	return w.Close()
}

func regionCode(i int) string { return fmt.Sprintf("R%02d", i) }

func writeCustomers(dir string, rows int, src rand.Source) (counts, error) {
	c := counts{rows: rows}
	w, err := newWriter(filepath.Join(dir, "customers.csv"))
	if err != nil {
		return c, err
	}
	rng := newRNG(src)

	// legacy_flag is written and never filled: a column that is entirely
	// empty is a defect a wide export produces constantly and nobody notices.
	w.header("customer_id", "name", "email", "signup_date", "region", "status",
		"phone", "city", "postcode", "credit_limit", "segment", "legacy_flag")

	var first, last string
	for i := range rows {
		id := fmt.Sprintf("CUS-%08d", i)
		if i >= rows-25 && rows > 100 {
			// The tail repeats earlier ids, so the key is a duplicate rather
			// than the table being short.
			id = fmt.Sprintf("CUS-%08d", i-(rows-25))
			c.duplicateKeys++
		}
		first = firstNames[rng.IntN(len(firstNames))]
		last = lastNames[rng.IntN(len(lastNames))]

		name := first + " " + last
		if i%9973 == 0 {
			name = "  " + name + "  "
			c.paddedNames++
		}

		date := isoDate(2019, i%2192)
		switch {
		case i%100003 == 0:
			date = "not a date"
			c.notADate++
		case i%1013 == 0:
			date = slashDate(2019, i%2192)
			c.altDateFormat++
		}

		region := regionCode(1 + i%regionRows)
		switch {
		case i%5003 == 0:
			region = "N/A"
			c.placeholders++
			c.orphanRegions++
		case i%7001 == 0:
			region = "-"
			c.placeholders++
			c.orphanRegions++
		case i%9001 == 0:
			// A code that is not in regions.csv at all, which is the defect
			// worth finding: it is not a placeholder anybody would notice.
			region = "R99"
			c.orphanRegions++
		}

		status := customerStatus[i%len(customerStatus)]
		if i%3001 == 0 {
			status = strings.ToLower(status)
			c.lowercaseStatus++
		}

		credit := 1000 + rng.IntN(50_000)
		if i%4001 == 0 {
			credit = -999
			c.numericSentinel++
		}

		w.field(id)
		w.field(name)
		w.field(strings.ToLower(first) + "." + strings.ToLower(last) + strconv.Itoa(i) + "@example.com")
		w.field(date)
		w.field(region)
		w.field(status)
		w.field(fmt.Sprintf("+1-555-%04d", i%10000))
		w.field(cities[i%len(cities)])
		w.field(fmt.Sprintf("%05d", 10000+i%500))
		w.fieldInt(credit)
		w.field(segments[i%len(segments)])
		w.field("")
		if err := w.end(); err != nil {
			return c, err
		}
	}

	// Exact duplicate rows, appended whole so that every column matches. The
	// row is built once and written twice: two rows that differ anywhere are
	// not what table.duplicate_rows is looking for.
	for i := range 40 {
		if rows <= i {
			break
		}
		row := []string{
			fmt.Sprintf("CUS-%08d", i),
			firstNames[0] + " " + lastNames[0],
			"duplicate" + strconv.Itoa(i) + "@example.com",
			isoDate(2019, i),
			regionCode(1 + i%regionRows),
			customerStatus[0],
			"+1-555-0000",
			cities[0],
			"10000",
			"1000",
			segments[0],
			"",
		}
		w.line(row...)
		w.line(row...)
		c.duplicateRows++
		c.rows += 2
	}
	return c, w.Close()
}

func writeOrders(dir string, rows, customers, products int, src rand.Source) (counts, error) {
	c := counts{rows: rows}
	w, err := newWriter(filepath.Join(dir, "orders.csv"))
	if err != nil {
		return c, err
	}
	rng := newRNG(src)

	w.header("order_id", "customer_id", "order_date", "ship_date", "amount",
		"currency", "status", "qty", "sku", "channel")

	for i := range rows {
		customer := fmt.Sprintf("CUS-%08d", rng.IntN(customers))
		if i%200003 == 0 {
			// A child row pointing at a parent that is not there. This is the
			// finding whose count the anti-join in relate.go has to produce.
			customer = "CUS-99999999"
			c.orphanParents++
		}

		orderDate := isoDate(2023, i%730)
		if i%500009 == 0 {
			orderDate = "1900-01-01"
			c.implausible++
		}
		shipDate := isoDate(2023, i%730+3)
		switch {
		case i%300007 == 0:
			shipDate = "2031-06-30"
			c.future++
		case i%700001 == 0:
			// Shipped before it was ordered. Both dates are valid, both are
			// plausible, and neither column is wrong on its own: nothing
			// deterministic proposes comparing them, which is what makes it
			// the agent's to find.
			shipDate = isoDate(2023, i%730-5)
			c.beforeOrder++
		}

		amount := float64(rng.IntN(400_000)) / 100
		if i%400009 == 0 {
			amount = -amount
			c.negative++
		}

		w.fieldInt(i + 1)
		w.field(customer)
		w.field(orderDate)
		w.field(shipDate)
		w.field(strconv.FormatFloat(amount, 'f', 2, 64))
		w.field("USD")
		w.field(orderStatus[i%len(orderStatus)])
		w.fieldInt(1 + rng.IntN(9))
		w.field(fmt.Sprintf("SKU-%06d", rng.IntN(products)))
		w.field(channels[i%len(channels)])
		if i%1000003 == 0 && i > 0 {
			// A stray comma: the row is wider than the header, which is how a
			// hand-edited export arrives.
			w.field("extra")
			c.raggedRows++
		}
		if err := w.end(); err != nil {
			return c, err
		}
	}
	return c, w.Close()
}

func writeProducts(dir string, rows int, src rand.Source) (counts, error) {
	c := counts{rows: rows}
	w, err := newWriter(filepath.Join(dir, "products.csv"))
	if err != nil {
		return c, err
	}
	rng := newRNG(src)

	w.header("sku", "product_name", "category", "unit_price", "weight_kg", "active")
	for i := range rows {
		w.field(fmt.Sprintf("SKU-%06d", i))
		w.field(fmt.Sprintf("Product %d", i))
		w.field(categories[i%len(categories)])
		w.field(strconv.FormatFloat(float64(rng.IntN(20_000))/100, 'f', 2, 64))
		w.field(strconv.FormatFloat(float64(rng.IntN(5000))/100, 'f', 2, 64))
		w.field("true")
		if err := w.end(); err != nil {
			return c, err
		}
	}
	return c, w.Close()
}

// writeMetrics is the wide table. Two hundred columns is what an analytics
// export looks like, and the profiler runs several queries per column, so it
// is the shape that turns a fast audit into a slow one.
func writeMetrics(dir string, rows int, src rand.Source) (counts, error) {
	c := counts{rows: rows}
	w, err := newWriter(filepath.Join(dir, "metrics.csv"))
	if err != nil {
		return c, err
	}
	rng := newRNG(src)

	cols := make([]string, 0, metricCols+1)
	cols = append(cols, "row_id")
	for i := 1; i <= metricCols; i++ {
		cols = append(cols, fmt.Sprintf("m%03d", i))
	}
	w.header(cols...)

	for i := range rows {
		w.fieldInt(i)
		for j := 1; j <= metricCols; j++ {
			switch j {
			case metricCols / 2:
				w.field("0") // constant
			case metricCols:
				w.field("") // never filled
			default:
				w.fieldInt(rng.IntN(1_000_000))
			}
		}
		if err := w.end(); err != nil {
			return c, err
		}
	}
	return c, w.Close()
}

// newRNG is where the generator's randomness comes from, and it is
// deliberately the reproducible kind. The seed is a flag so that a measurement
// taken on one machine can be repeated on another, and nothing generated here
// is a secret: it is fixture data whose whole purpose is to be regenerated
// byte for byte.
func newRNG(src rand.Source) *rand.Rand {
	return rand.New(src) //nolint:gosec // reproducibility is the requirement, not unpredictability
}

func isoDate(year, offset int) string {
	return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, offset).Format("2006-01-02")
}

func slashDate(year, offset int) string {
	return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, offset).Format("02/01/2006")
}

var (
	firstNames     = []string{"Ana", "Ben", "Chi", "Dev", "Eve", "Fay", "Gus", "Hal", "Ivy", "Jon"}
	lastNames      = []string{"Adams", "Baker", "Chen", "Diaz", "Evans", "Frost", "Gupta", "Hall"}
	cities         = []string{"Austin", "Berlin", "Cairo", "Dublin", "Edinburgh", "Faro", "Genoa"}
	segments       = []string{"standard", "premium", "enterprise"}
	customerStatus = []string{"Active", "Closed", "Pending"}
	orderStatus    = []string{"placed", "shipped", "delivered", "canceled"}
	channels       = []string{"web", "phone", "partner", "store"}
	categories     = []string{"tools", "garden", "office", "kitchen", "outdoor"}
)

func dirBytes(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func humanBytes(n int64) string {
	switch {
	case n > 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n > 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
