package index

import "context"

// FakeWalker serves pre-supplied channels for use in tests.
type FakeWalker struct {
	records <-chan FileRecord
	errs    <-chan error
}

func NewFakeWalker(records <-chan FileRecord, errs <-chan error) *FakeWalker {
	return &FakeWalker{records: records, errs: errs}
}

func (f *FakeWalker) Walk(_ context.Context, _ string) (<-chan FileRecord, <-chan error) {
	return f.records, f.errs
}
