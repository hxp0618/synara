package billing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrInvoiceNotFound = errors.New("provider cost invoice not found")

type Adapter interface {
	FetchActualInvoice(context.Context, ImportActualInvoiceRequest) (ImportedActualInvoice, error)
}

type ImportedActualInvoice struct {
	ExternalImportID     string
	BillingPeriodStartAt time.Time
	BillingPeriodEndAt   time.Time
	CurrencyCode         string
	Lines                []ImportedActualInvoiceLine
	SourceProvenance     *InvoiceSourceProvenance
}

type InvoiceSourceProvenance struct {
	Format                         ExportObjectFormat    `json:"format"`
	BundleChecksum                 string                `json:"bundleChecksum"`
	Manifest                       InvoiceSourceObject   `json:"manifest"`
	Chunks                         []InvoiceSourceObject `json:"chunks"`
	CompressedBytes                int64                 `json:"compressedBytes"`
	DecompressedBytes              int64                 `json:"decompressedBytes"`
	RowCount                       int                   `json:"rowCount"`
	FilteredAdjustmentCount        int                   `json:"filteredAdjustmentCount"`
	FilteredAdjustmentAmountMicros int64                 `json:"filteredAdjustmentAmountMicros"`
}

type InvoiceSourceObject struct {
	Key          string    `json:"key"`
	Version      string    `json:"version"`
	ETag         string    `json:"etag"`
	LastModified time.Time `json:"lastModified"`
	SizeBytes    int64     `json:"sizeBytes"`
	SHA256       string    `json:"sha256"`
}

type ImportedActualInvoiceLine struct {
	ExternalLineID         string
	ChargeKind             string
	ResourceCorrelationKey string
	BillingPeriodStartAt   time.Time
	BillingPeriodEndAt     time.Time
	CurrencyCode           string
	AmountMicros           int64
}

type FixtureInvoice struct {
	TenantID uuid.UUID
	Provider string
	Invoice  ImportedActualInvoice
}

type FixtureAdapter struct {
	invoices map[string]ImportedActualInvoice
}

func NewFixtureAdapter(fixtures ...FixtureInvoice) *FixtureAdapter {
	index := make(map[string]ImportedActualInvoice, len(fixtures))
	for _, fixture := range fixtures {
		index[fixtureKey(fixture.TenantID, fixture.Provider, fixture.Invoice.ExternalImportID)] = cloneImportedInvoice(fixture.Invoice)
	}
	return &FixtureAdapter{invoices: index}
}

func (a *FixtureAdapter) FetchActualInvoice(
	_ context.Context,
	request ImportActualInvoiceRequest,
) (ImportedActualInvoice, error) {
	if a == nil {
		return ImportedActualInvoice{}, ErrInvoiceNotFound
	}
	invoice, found := a.invoices[fixtureKey(request.TenantID, request.Provider, request.ExternalImportID)]
	if !found {
		return ImportedActualInvoice{}, ErrInvoiceNotFound
	}
	return cloneImportedInvoice(invoice), nil
}

func fixtureKey(tenantID uuid.UUID, provider, externalImportID string) string {
	return tenantID.String() + "\x00" + strings.ToLower(strings.TrimSpace(provider)) + "\x00" + strings.TrimSpace(externalImportID)
}

func cloneImportedInvoice(invoice ImportedActualInvoice) ImportedActualInvoice {
	cloned := invoice
	if invoice.SourceProvenance != nil {
		provenance := *invoice.SourceProvenance
		provenance.Chunks = append([]InvoiceSourceObject(nil), invoice.SourceProvenance.Chunks...)
		cloned.SourceProvenance = &provenance
	}
	if len(invoice.Lines) == 0 {
		cloned.Lines = nil
		return cloned
	}
	cloned.Lines = make([]ImportedActualInvoiceLine, len(invoice.Lines))
	copy(cloned.Lines, invoice.Lines)
	return cloned
}
