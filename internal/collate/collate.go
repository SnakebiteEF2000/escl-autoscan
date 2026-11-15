// Package collate turns the two single-sided passes of a double-sided scan
// into one PDF.
//
// A feeder that scans one side only is used twice: first for the fronts,
// then, with the whole stack turned over, for the backs. Turning the stack
// over reverses it, so the backs arrive last sheet first:
//
//	fronts: 1 3 5      backs: 6 4 2      result: 1 2 3 4 5 6
//
// The PDFs are parsed and written by pdfcpu. Pages are moved as they are:
// nothing is re-encoded, optimized or decoded.
package collate

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// PageCountError is returned by Interleave when the passes differ in length,
// e.g. because the feeder pulled two sheets at once.
type PageCountError struct{ Fronts, Backs int }

func (e *PageCountError) Error() string {
	return fmt.Sprintf("%d front pages but %d back pages", e.Fronts, e.Backs)
}

// config returns the pdfcpu configuration: no configuration directory (the
// container's root file system is read-only), no optimization, and a classic
// cross-reference table without object streams, which every consumer reads
// and which output.ValidatePDF can count pages in.
func config() *model.Configuration {
	c := model.NewStatelessConfiguration()
	c.Optimize = false
	c.OptimizeBeforeWriting = false
	c.OptimizeResourceDicts = false
	c.WriteObjectStream = false
	c.WriteXRefStream = false
	c.CreateBookmarks = false
	return c
}

// Pages returns the total number of pages of docs. An error means that
// pdfcpu cannot read one of them, so they cannot be collated.
func Pages(docs [][]byte) (int, error) {
	conf := config()
	total := 0
	for i, doc := range docs {
		n, err := api.PageCount(context.Background(), bytes.NewReader(doc), conf)
		if err != nil {
			return 0, fmt.Errorf("document %d: %w", i+1, err)
		}
		total += n
	}
	return total, nil
}

// Interleave merges the fronts and the backs of a stack into one PDF: the
// pages of fronts in order, each followed by the matching page of backs,
// which are taken in reverse order. A pass may consist of several documents
// (some scanners return one per page); they count as one sequence.
//
// It returns the PDF and its page count, or a *PageCountError when the passes
// do not have the same number of pages.
func Interleave(fronts, backs [][]byte) ([]byte, int, error) {
	if len(fronts) == 0 || len(backs) == 0 {
		return nil, 0, errors.New("collate: missing pass")
	}
	c := context.Background()
	conf := config()
	conf.Cmd = model.MERGECREATE

	// All documents into one context: fronts 1..n, then backs n+1..n+m.
	var dest *model.Context
	n := 0
	for i, doc := range append(append([][]byte{}, fronts...), backs...) {
		if i == len(fronts) {
			n = dest.PageCount
		}
		src, err := api.ReadAndValidate(c, bytes.NewReader(doc), conf)
		if err != nil {
			return nil, 0, fmt.Errorf("collate: document %d: %w", i+1, err)
		}
		if dest == nil {
			dest = src
			dest.EnsureVersionForWriting()
			continue
		}
		if err := pdfcpu.MergeXRefTables(c, "", src, dest, false, false); err != nil {
			return nil, 0, fmt.Errorf("collate: document %d: %w", i+1, err)
		}
	}
	m := dest.PageCount - n
	if n != m {
		return nil, 0, &PageCountError{Fronts: n, Backs: m}
	}

	order := make([]int, 0, n+m)
	for i := 1; i <= n; i++ {
		order = append(order, i, n+m+1-i)
	}
	out, err := pdfcpu.ExtractPages(c, dest, order, false)
	if err != nil {
		return nil, 0, fmt.Errorf("collate: %w", err)
	}
	var buf bytes.Buffer
	size := 0
	for _, doc := range fronts {
		size += len(doc)
	}
	for _, doc := range backs {
		size += len(doc)
	}
	buf.Grow(size + size/64 + 64<<10) // the result is about as large as the input
	if err := api.WriteContext(c, out, &buf); err != nil {
		return nil, 0, fmt.Errorf("collate: %w", err)
	}
	return buf.Bytes(), n + m, nil
}
