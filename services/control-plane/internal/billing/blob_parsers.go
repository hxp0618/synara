package billing

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type parsedInvoice struct {
	BillingPeriodStartAt     time.Time
	BillingPeriodEndAt       time.Time
	CurrencyCode             string
	Rows                     []parsedInvoiceRow
	FilteredAdjustmentCount  int
	FilteredAdjustmentAmount *big.Rat
}

type parsedInvoiceRow struct {
	RawExternalLineID    string
	BillingPeriodStartAt time.Time
	BillingPeriodEndAt   time.Time
	CurrencyCode         string
	ChargeKind           string
	ResourceKey          string
	Amount               *big.Rat
}

type aggregatedInvoiceLine struct {
	RawExternalLineIDs []string
	ChargeKind         string
	ResourceKey        string
	Amount             *big.Rat
}

type jsonRowsEnvelope struct {
	Rows []map[string]any
	Meta map[string]any
}

var dateOnlyPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
var awsAccountIDPattern = regexp.MustCompile(`^\d{12}$`)
var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z0-9-]+-\d+$`)

func parseImportedInvoiceBlob(
	provider string,
	externalImportID string,
	format ExportObjectFormat,
	blob []byte,
) (ImportedActualInvoice, error) {
	parsed, err := parseInvoiceByFormat(format, blob)
	if err != nil {
		return ImportedActualInvoice{}, err
	}
	return buildImportedInvoice(provider, externalImportID, parsed)
}

func parseInvoiceByFormat(format ExportObjectFormat, blob []byte) (parsedInvoice, error) {
	switch format {
	case ExportObjectFormatAWSCURCSV:
		return parseAWSCURCSV(blob)
	case ExportObjectFormatGCPBillingJSON:
		return parseGCPBillingJSON(blob)
	case ExportObjectFormatAzureCostCSV:
		return parseAzureNormalizedCSV(blob)
	case ExportObjectFormatAzureCostJSON:
		return parseAzureNormalizedJSON(blob)
	default:
		return parsedInvoice{}, fmt.Errorf("unsupported billing export object format %q", format)
	}
}

func buildImportedInvoice(
	provider string,
	externalImportID string,
	parsed parsedInvoice,
) (ImportedActualInvoice, error) {
	if len(parsed.Rows) == 0 {
		if parsed.BillingPeriodStartAt.IsZero() || parsed.BillingPeriodEndAt.IsZero() || strings.TrimSpace(parsed.CurrencyCode) == "" {
			return ImportedActualInvoice{}, fmt.Errorf("billing export %s/%s produced no rows and no invoice metadata", provider, externalImportID)
		}
		currencyCode, err := normalizeCurrency(parsed.CurrencyCode)
		if err != nil {
			return ImportedActualInvoice{}, err
		}
		return ImportedActualInvoice{
			ExternalImportID:     externalImportID,
			BillingPeriodStartAt: parsed.BillingPeriodStartAt.UTC(),
			BillingPeriodEndAt:   parsed.BillingPeriodEndAt.UTC(),
			CurrencyCode:         currencyCode,
			Lines:                nil,
		}, nil
	}

	invoicePeriodStartAt := parsed.BillingPeriodStartAt
	invoicePeriodEndAt := parsed.BillingPeriodEndAt
	invoiceCurrency := strings.TrimSpace(parsed.CurrencyCode)

	rawLineIDs := make(map[string]struct{}, len(parsed.Rows))
	aggregated := make(map[string]*aggregatedInvoiceLine, len(parsed.Rows))
	for _, row := range parsed.Rows {
		lineID := strings.TrimSpace(row.RawExternalLineID)
		if lineID == "" {
			return ImportedActualInvoice{}, fmt.Errorf("billing export %s/%s contains a row without an external line id", provider, externalImportID)
		}
		if _, exists := rawLineIDs[lineID]; exists {
			return ImportedActualInvoice{}, fmt.Errorf("billing export %s/%s repeats external line id %q", provider, externalImportID, lineID)
		}
		rawLineIDs[lineID] = struct{}{}

		rowPeriodStartAt, rowPeriodEndAt, err := normalizeClosedPeriod(row.BillingPeriodStartAt, row.BillingPeriodEndAt)
		if err != nil {
			return ImportedActualInvoice{}, fmt.Errorf("billing export %s/%s contains an invalid billing period for line %q: %w",
				provider, externalImportID, lineID, err)
		}
		rowCurrency, err := normalizeCurrency(row.CurrencyCode)
		if err != nil {
			return ImportedActualInvoice{}, fmt.Errorf("billing export %s/%s contains an invalid currency for line %q: %w",
				provider, externalImportID, lineID, err)
		}
		chargeKind, err := normalizeChargeKind(row.ChargeKind)
		if err != nil {
			return ImportedActualInvoice{}, fmt.Errorf("billing export %s/%s contains an invalid charge kind for line %q: %w",
				provider, externalImportID, lineID, err)
		}
		resourceKey := normalizeResourceCorrelationKey(row.ResourceKey)
		if resourceKey == "" {
			return ImportedActualInvoice{}, fmt.Errorf("billing export %s/%s contains an empty resource key for line %q", provider, externalImportID, lineID)
		}
		if row.Amount == nil {
			return ImportedActualInvoice{}, fmt.Errorf("billing export %s/%s contains an empty amount for line %q", provider, externalImportID, lineID)
		}

		if invoicePeriodStartAt.IsZero() && invoicePeriodEndAt.IsZero() {
			invoicePeriodStartAt = rowPeriodStartAt
			invoicePeriodEndAt = rowPeriodEndAt
		}
		if invoiceCurrency == "" {
			invoiceCurrency = rowCurrency
		}
		if !rowPeriodStartAt.Equal(invoicePeriodStartAt) || !rowPeriodEndAt.Equal(invoicePeriodEndAt) {
			return ImportedActualInvoice{}, fmt.Errorf("billing export %s/%s mixes billing periods", provider, externalImportID)
		}
		if rowCurrency != invoiceCurrency {
			return ImportedActualInvoice{}, fmt.Errorf("billing export %s/%s mixes currencies", provider, externalImportID)
		}

		groupKey := resourceKey + "\x00" + chargeKind
		group, exists := aggregated[groupKey]
		if !exists {
			group = &aggregatedInvoiceLine{
				ChargeKind:  chargeKind,
				ResourceKey: resourceKey,
				Amount:      new(big.Rat),
			}
			aggregated[groupKey] = group
		}
		group.RawExternalLineIDs = append(group.RawExternalLineIDs, lineID)
		group.Amount.Add(group.Amount, row.Amount)
	}

	lines := make([]ImportedActualInvoiceLine, 0, len(aggregated))
	for _, group := range aggregated {
		slices.Sort(group.RawExternalLineIDs)
		amountMicros, err := decimalRatToRoundedMicros(group.Amount)
		if err != nil {
			return ImportedActualInvoice{}, fmt.Errorf("billing export %s/%s could not be represented in micros: %w", provider, externalImportID, err)
		}
		lines = append(lines, ImportedActualInvoiceLine{
			ExternalLineID:         aggregatedExternalLineID(provider, externalImportID, group),
			ChargeKind:             group.ChargeKind,
			ResourceCorrelationKey: group.ResourceKey,
			BillingPeriodStartAt:   invoicePeriodStartAt,
			BillingPeriodEndAt:     invoicePeriodEndAt,
			CurrencyCode:           invoiceCurrency,
			AmountMicros:           amountMicros,
		})
	}
	slices.SortFunc(lines, func(left, right ImportedActualInvoiceLine) int {
		switch {
		case left.ExternalLineID < right.ExternalLineID:
			return -1
		case left.ExternalLineID > right.ExternalLineID:
			return 1
		default:
			return 0
		}
	})

	return ImportedActualInvoice{
		ExternalImportID:     externalImportID,
		BillingPeriodStartAt: invoicePeriodStartAt,
		BillingPeriodEndAt:   invoicePeriodEndAt,
		CurrencyCode:         invoiceCurrency,
		Lines:                lines,
	}, nil
}

func aggregatedExternalLineID(
	provider string,
	externalImportID string,
	group *aggregatedInvoiceLine,
) string {
	if len(group.RawExternalLineIDs) == 1 {
		return group.RawExternalLineIDs[0]
	}
	hasher := sha256.New()
	writeChecksumPart(hasher, provider)
	writeChecksumPart(hasher, externalImportID)
	writeChecksumPart(hasher, group.ResourceKey)
	writeChecksumPart(hasher, group.ChargeKind)
	for _, rawID := range group.RawExternalLineIDs {
		writeChecksumPart(hasher, rawID)
	}
	return "agg-" + hex.EncodeToString(hasher.Sum(nil)[:12])
}

func parseAWSCURCSV(blob []byte) (parsedInvoice, error) {
	records, err := parseCSVRecords(blob)
	if err != nil {
		return parsedInvoice{}, err
	}
	return parseAWSCURRecords(records, false)
}

func parseAWSCURRecords(records []map[string]string, isCUR2 bool) (parsedInvoice, error) {
	parentResourceIDs := make(map[string]struct{})
	replacementBoundaries := make(map[string]*big.Rat)
	parentBoundaryCounts := make(map[string]int)
	for index, record := range records {
		parentResourceID := normalizeProviderResourceID(record["splitlineitemparentresourceid"])
		if parentResourceID != "" {
			parentResourceIDs[parentResourceID] = struct{}{}
			boundary, ok := awsCURReplacementBoundary(record, parentResourceID)
			if !ok {
				return parsedInvoice{}, fmt.Errorf("aws cur row %d: split child replacement boundary is incomplete", index+2)
			}
			amount, err := awsCURCost(record)
			if err != nil {
				return parsedInvoice{}, fmt.Errorf("aws cur row %d: %w", index+2, err)
			}
			if replacementBoundaries[boundary] == nil {
				replacementBoundaries[boundary] = new(big.Rat)
			}
			replacementBoundaries[boundary].Add(replacementBoundaries[boundary], amount)
		} else if awsCURReplaceableParent(record) {
			resourceID := normalizeProviderResourceID(record["lineitemresourceid"])
			if boundary, ok := awsCURReplacementBoundary(record, resourceID); ok {
				parentBoundaryCounts[boundary]++
			}
		}
	}
	for boundary := range replacementBoundaries {
		if parentBoundaryCounts[boundary] != 1 {
			return parsedInvoice{}, fmt.Errorf(
				"aws cur split replacement boundary has %d parent rows, want exactly 1",
				parentBoundaryCounts[boundary],
			)
		}
	}
	invoice := parsedInvoice{FilteredAdjustmentAmount: new(big.Rat)}
	rawLineIDs := make(map[string]struct{}, len(records))
	rows := make([]parsedInvoiceRow, 0, len(records))
	for index, record := range records {
		resourceID := normalizeProviderResourceID(record["lineitemresourceid"])
		parentResourceID := normalizeProviderResourceID(record["splitlineitemparentresourceid"])
		replacementBoundary := ""
		if parentResourceID == "" && awsCURReplaceableParent(record) {
			if _, referencedBySplitChildren := parentResourceIDs[resourceID]; referencedBySplitChildren {
				boundary, ok := awsCURReplacementBoundary(record, resourceID)
				if !ok {
					return parsedInvoice{}, fmt.Errorf("aws cur row %d: referenced parent replacement boundary is incomplete", index+2)
				}
				if _, replaced := replacementBoundaries[boundary]; replaced {
					replacementBoundary = boundary
				}
				if replacementBoundary == "" {
					record["synaraallocationquality"] = "split-parent-not-replaced"
				}
			}
		}
		lineID, err := requireField(record, "identitylineitemid")
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("aws cur row %d: %w", index+2, err)
		}
		if _, duplicate := rawLineIDs[lineID]; duplicate {
			return parsedInvoice{}, fmt.Errorf("aws cur row %d: duplicate external line id %q", index+2, lineID)
		}
		rawLineIDs[lineID] = struct{}{}
		periodStartAt, err := parseBillingTimeForField(record, "billbillingperiodstartdate")
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("aws cur row %d: %w", index+2, err)
		}
		periodEndAt, err := parseBillingTimeForField(record, "billbillingperiodenddate")
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("aws cur row %d: %w", index+2, err)
		}
		currencyCode, err := requireField(record, "lineitemcurrencycode")
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("aws cur row %d: %w", index+2, err)
		}
		amount, err := awsCURCost(record)
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("aws cur row %d: %w", index+2, err)
		}
		if replacementBoundary != "" {
			if parentBoundaryCounts[replacementBoundary] != 1 {
				return parsedInvoice{}, fmt.Errorf("aws cur row %d: split replacement boundary has %d parent rows", index+2, parentBoundaryCounts[replacementBoundary])
			}
			if replacementBoundaries[replacementBoundary].Cmp(amount) != 0 {
				return parsedInvoice{}, fmt.Errorf("aws cur row %d: split children do not exactly replace parent cost", index+2)
			}
			continue
		}
		if isCUR2 && resourceID == "" && awsCURAccountAdjustment(record) {
			if firstNonEmptyField(record, "lineitemusageaccountid", "billpayeraccountid") == "" {
				return parsedInvoice{}, fmt.Errorf("aws cur row %d: account-level adjustment is missing an account id", index+2)
			}
			if amount.Sign() > 0 {
				return parsedInvoice{}, fmt.Errorf("aws cur row %d: account-level adjustment must not be positive", index+2)
			}
			if err := setParsedInvoiceMetadata(&invoice, periodStartAt, periodEndAt, currencyCode); err != nil {
				return parsedInvoice{}, fmt.Errorf("aws cur row %d: %w", index+2, err)
			}
			invoice.FilteredAdjustmentCount++
			invoice.FilteredAdjustmentAmount.Add(invoice.FilteredAdjustmentAmount, amount)
			continue
		}
		chargeKind, err := normalizeImportedChargeKind(firstNonEmpty(
			record["resourcetagsusersynarachargekind"],
			record["lineitemusagetype"],
			record["lineitemoperation"],
			record["lineitemlineitemdescription"],
			record["productusagetype"],
			record["pricingunit"],
		))
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("aws cur row %d: %w", index+2, err)
		}

		region := firstNonEmpty(
			record["productregion"],
			awsRegionFromAZ(record["lineitemavailabilityzone"]),
			awsRegionFromResourceID(record["lineitemresourceid"]),
		)
		resourceKey, err := buildAWSResourceKey(record, region, isCUR2)
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("aws cur row %d: %w", index+2, err)
		}

		rows = append(rows, parsedInvoiceRow{
			RawExternalLineID:    lineID,
			BillingPeriodStartAt: periodStartAt,
			BillingPeriodEndAt:   periodEndAt,
			CurrencyCode:         currencyCode,
			ChargeKind:           chargeKind,
			ResourceKey:          resourceKey,
			Amount:               amount,
		})
		if err := setParsedInvoiceMetadata(&invoice, periodStartAt, periodEndAt, currencyCode); err != nil {
			return parsedInvoice{}, fmt.Errorf("aws cur row %d: %w", index+2, err)
		}
	}
	invoice.Rows = rows
	return invoice, nil
}

func awsCURReplacementBoundary(record map[string]string, resourceID string) (string, bool) {
	usageStart := firstNonEmptyField(record, "lineitemusagestartdate")
	usageEnd := firstNonEmptyField(record, "lineitemusageenddate")
	if usageStart == "" || usageEnd == "" {
		interval := strings.Split(firstNonEmptyField(record, "identitytimeinterval"), "/")
		if len(interval) == 2 {
			usageStart, usageEnd = interval[0], interval[1]
		}
	}
	operation := firstNonEmptyField(record, "lineitemoperation")
	productCode := firstNonEmptyField(record, "lineitemproductcode")
	_, costFamily, err := awsCURCostAndFamily(record)
	if resourceID == "" || usageStart == "" || usageEnd == "" || operation == "" || productCode == "" || err != nil {
		return "", false
	}
	return strings.Join([]string{
		normalizeProviderResourceID(resourceID), strings.TrimSpace(usageStart), strings.TrimSpace(usageEnd),
		strings.ToLower(operation), strings.ToLower(productCode), costFamily,
	}, "\x00"), true
}

func awsCURAccountAdjustment(record map[string]string) bool {
	lineItemType := strings.ToLower(firstNonEmptyField(record, "lineitemlineitemtype"))
	return lineItemType == "credit" || lineItemType == "refund" || lineItemType == "savingsplannegation" ||
		(strings.HasSuffix(lineItemType, "discount") && lineItemType != "discountedusage")
}

func setParsedInvoiceMetadata(invoice *parsedInvoice, startAt, endAt time.Time, currency string) error {
	if invoice.BillingPeriodStartAt.IsZero() {
		invoice.BillingPeriodStartAt, invoice.BillingPeriodEndAt, invoice.CurrencyCode = startAt, endAt, currency
		return nil
	}
	if !invoice.BillingPeriodStartAt.Equal(startAt) || !invoice.BillingPeriodEndAt.Equal(endAt) {
		return errors.New("aws cur mixes billing periods")
	}
	if !strings.EqualFold(invoice.CurrencyCode, currency) {
		return errors.New("aws cur mixes currencies")
	}
	return nil
}

func awsCURReplaceableParent(record map[string]string) bool {
	switch strings.ToLower(firstNonEmptyField(record, "lineitemlineitemtype")) {
	case "", "usage", "discountedusage", "savingsplancoveredusage":
		return true
	default:
		return false
	}
}

func awsCURCost(record map[string]string) (*big.Rat, error) {
	amount, _, err := awsCURCostAndFamily(record)
	return amount, err
}

func awsCURCostAndFamily(record map[string]string) (*big.Rat, string, error) {
	if firstNonEmptyField(record, "splitlineitemparentresourceid") != "" {
		return awsCURSplitCost(record)
	}
	for _, candidate := range []struct {
		field, family string
	}{
		{"reservationneteffectivecost", "net"},
		{"reservationeffectivecost", "non-net"},
		{"savingsplannetsavingsplaneffectivecost", "net"},
		{"savingsplansavingsplaneffectivecost", "non-net"},
		{"lineitemnetunblendedcost", "net"},
		{"lineitemunblendedcost", "non-net"},
	} {
		amount, found, err := parseOptionalDecimalField(record, candidate.field)
		if err != nil {
			return nil, "", err
		}
		if found {
			return amount, candidate.family, nil
		}
	}
	return nil, "", errors.New("missing required cost")
}

func awsCURSplitCost(record map[string]string) (*big.Rat, string, error) {
	netSplit, hasNetSplit, err := parseOptionalDecimalField(record, "splitlineitemnetsplitcost")
	if err != nil {
		return nil, "", err
	}
	netUnused, hasNetUnused, err := parseOptionalDecimalField(record, "splitlineitemnetunusedcost")
	if err != nil {
		return nil, "", err
	}
	split, hasSplit, err := parseOptionalDecimalField(record, "splitlineitemsplitcost")
	if err != nil {
		return nil, "", err
	}
	unused, hasUnused, err := parseOptionalDecimalField(record, "splitlineitemunusedcost")
	if err != nil {
		return nil, "", err
	}
	switch {
	case hasNetSplit:
		if !hasNetUnused && hasUnused {
			return nil, "", errors.New("net split cost cannot be combined with non-net unused cost")
		}
		if !hasNetUnused {
			netUnused = new(big.Rat)
		}
		return new(big.Rat).Add(netSplit, netUnused), "net", nil
	case hasNetUnused:
		return nil, "", errors.New("net unused cost requires net split cost")
	case hasSplit:
		if !hasUnused {
			unused = new(big.Rat)
		}
		return new(big.Rat).Add(split, unused), "non-net", nil
	default:
		return nil, "", errors.New("missing required split cost")
	}
}

func parseOptionalDecimalField(record map[string]string, alias string) (*big.Rat, bool, error) {
	value := strings.TrimSpace(record[canonicalFieldName(alias)])
	if value == "" {
		return nil, false, nil
	}
	parsed, err := parseDecimalRat(value)
	return parsed, true, err
}

func parseGCPBillingJSON(blob []byte) (parsedInvoice, error) {
	envelope, err := decodeJSONRows(blob)
	if err != nil {
		return parsedInvoice{}, err
	}
	rows := make([]parsedInvoiceRow, 0, len(envelope.Rows))
	for index, object := range envelope.Rows {
		record := flattenJSONMap(object)
		lineID, err := requireField(record, "lineitemid", "lineitem_id", "id")
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("gcp billing row %d: %w", index+1, err)
		}

		periodStartAt, periodEndAt, err := gcpBillingPeriod(record)
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("gcp billing row %d: %w", index+1, err)
		}
		currencyCode, err := requireField(record, "currency", "currencycode", "currency_code")
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("gcp billing row %d: %w", index+1, err)
		}
		amount, err := parseDecimalField(record, "cost", "costamount", "costinbillingcurrency")
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("gcp billing row %d: %w", index+1, err)
		}
		chargeKind, err := normalizeImportedChargeKind(firstNonEmpty(
			record["chargekind"],
			record["charge_kind"],
			record["labelschargekind"],
			record["systemlabelschargekind"],
			record["skudescription"],
			record["usageunit"],
		))
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("gcp billing row %d: %w", index+1, err)
		}
		region := firstNonEmpty(
			record["locationregion"],
			record["locationlocation"],
			record["location"],
			record["resourcelocation"],
			gcpLocationFromGlobalName(record["resourceglobalname"]),
		)
		resourceKey, err := buildGCPResourceKey(record, region)
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("gcp billing row %d: %w", index+1, err)
		}

		rows = append(rows, parsedInvoiceRow{
			RawExternalLineID:    lineID,
			BillingPeriodStartAt: periodStartAt,
			BillingPeriodEndAt:   periodEndAt,
			CurrencyCode:         currencyCode,
			ChargeKind:           chargeKind,
			ResourceKey:          resourceKey,
			Amount:               amount,
		})
	}
	return parsedInvoice{Rows: rows}, nil
}

func parseAzureNormalizedCSV(blob []byte) (parsedInvoice, error) {
	records, err := parseCSVRecords(blob)
	if err != nil {
		return parsedInvoice{}, err
	}
	rows := make([]parsedInvoiceRow, 0, len(records))
	for index, record := range records {
		row, err := buildAzureParsedRow(record)
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("azure csv row %d: %w", index+2, err)
		}
		rows = append(rows, row)
	}
	return parsedInvoice{Rows: rows}, nil
}

func parseAzureNormalizedJSON(blob []byte) (parsedInvoice, error) {
	envelope, err := decodeJSONRows(blob)
	if err != nil {
		return parsedInvoice{}, err
	}
	meta := flattenJSONMap(envelope.Meta)
	rows := make([]parsedInvoiceRow, 0, len(envelope.Rows))
	for index, object := range envelope.Rows {
		record := flattenJSONMap(object)
		for key, value := range meta {
			if _, exists := record[key]; !exists {
				record[key] = value
			}
		}
		row, err := buildAzureParsedRow(record)
		if err != nil {
			return parsedInvoice{}, fmt.Errorf("azure json row %d: %w", index+1, err)
		}
		rows = append(rows, row)
	}
	invoice := parsedInvoice{Rows: rows}
	if len(meta) > 0 {
		if rawStartAt := firstNonEmpty(meta["billingperiodstartat"], meta["billingperiodstart"]); rawStartAt != "" {
			invoice.BillingPeriodStartAt, err = parseBillingTime(rawStartAt)
			if err != nil {
				return parsedInvoice{}, fmt.Errorf("azure json metadata: %w", err)
			}
		}
		if rawEndAt := firstNonEmpty(meta["billingperiodendat"], meta["billingperiodend"]); rawEndAt != "" {
			invoice.BillingPeriodEndAt, err = parseBillingTime(rawEndAt)
			if err != nil {
				return parsedInvoice{}, fmt.Errorf("azure json metadata: %w", err)
			}
		}
		invoice.CurrencyCode = firstNonEmpty(meta["currencycode"], meta["currency"])
	}
	return invoice, nil
}

func buildAzureParsedRow(record map[string]string) (parsedInvoiceRow, error) {
	lineID, err := requireField(record, "externallineid", "lineitemid", "lineitem_id", "id")
	if err != nil {
		return parsedInvoiceRow{}, err
	}
	periodStartAt, err := parseBillingTimeForField(record, "billingperiodstartat", "billingperiodstart")
	if err != nil {
		return parsedInvoiceRow{}, err
	}
	periodEndAt, err := parseBillingTimeForField(record, "billingperiodendat", "billingperiodend")
	if err != nil {
		return parsedInvoiceRow{}, err
	}
	currencyCode, err := requireField(record, "currencycode", "currency")
	if err != nil {
		return parsedInvoiceRow{}, err
	}
	amount, err := parseDecimalField(record, "costinbillingcurrency", "amount", "cost")
	if err != nil {
		return parsedInvoiceRow{}, err
	}
	chargeKind, err := normalizeImportedChargeKind(firstNonEmpty(record["chargekind"], record["charge_kind"]))
	if err != nil {
		return parsedInvoiceRow{}, err
	}
	resourceKey, err := buildAzureResourceKey(record)
	if err != nil {
		return parsedInvoiceRow{}, err
	}
	return parsedInvoiceRow{
		RawExternalLineID:    lineID,
		BillingPeriodStartAt: periodStartAt,
		BillingPeriodEndAt:   periodEndAt,
		CurrencyCode:         currencyCode,
		ChargeKind:           chargeKind,
		ResourceKey:          resourceKey,
		Amount:               amount,
	}, nil
}

func parseCSVRecords(blob []byte) ([]map[string]string, error) {
	reader := csv.NewReader(bytes.NewReader(blob))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("billing csv header: %w", err)
	}
	headerKeys := make([]string, len(header))
	for index, value := range header {
		headerKeys[index] = canonicalFieldName(value)
	}

	var records []map[string]string
	for {
		row, err := reader.Read()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, fmt.Errorf("billing csv row: %w", err)
		}
		record := make(map[string]string, len(headerKeys))
		for index, key := range headerKeys {
			if key == "" {
				continue
			}
			if index < len(row) {
				record[key] = strings.TrimSpace(row[index])
			}
		}
		if isAllEmpty(record) {
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func decodeJSONRows(blob []byte) (jsonRowsEnvelope, error) {
	trimmed := bytes.TrimSpace(blob)
	if len(trimmed) == 0 {
		return jsonRowsEnvelope{}, fmt.Errorf("billing json export is empty")
	}

	switch trimmed[0] {
	case '[':
		var rows []map[string]any
		if err := decodeJSONValue(trimmed, &rows); err != nil {
			return jsonRowsEnvelope{}, fmt.Errorf("billing json array: %w", err)
		}
		return jsonRowsEnvelope{Rows: rows}, nil
	case '{':
		var object map[string]any
		if err := decodeJSONValue(trimmed, &object); err != nil {
			return jsonRowsEnvelope{}, fmt.Errorf("billing json object: %w", err)
		}
		for _, arrayKey := range []string{"items", "rows"} {
			if rawRows, ok := object[arrayKey]; ok {
				entries, ok := rawRows.([]any)
				if !ok {
					return jsonRowsEnvelope{}, fmt.Errorf("billing json field %q must be an array", arrayKey)
				}
				rows := make([]map[string]any, 0, len(entries))
				for _, entry := range entries {
					record, ok := entry.(map[string]any)
					if !ok {
						return jsonRowsEnvelope{}, fmt.Errorf("billing json field %q contains a non-object row", arrayKey)
					}
					rows = append(rows, record)
				}
				meta := make(map[string]any, len(object))
				for key, value := range object {
					if key == arrayKey {
						continue
					}
					meta[key] = value
				}
				return jsonRowsEnvelope{Rows: rows, Meta: meta}, nil
			}
		}
		return jsonRowsEnvelope{Rows: []map[string]any{object}}, nil
	default:
		lines := strings.Split(string(trimmed), "\n")
		rows := make([]map[string]any, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var object map[string]any
			if err := decodeJSONValue([]byte(line), &object); err != nil {
				return jsonRowsEnvelope{}, fmt.Errorf("billing ndjson row: %w", err)
			}
			rows = append(rows, object)
		}
		if len(rows) == 0 {
			return jsonRowsEnvelope{}, fmt.Errorf("billing json export is empty")
		}
		return jsonRowsEnvelope{Rows: rows}, nil
	}
}

func flattenJSONMap(object map[string]any) map[string]string {
	flat := make(map[string]string)
	flattenJSONInto("", object, flat)
	return flat
}

func flattenJSONInto(prefix string, value any, flat map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			nextPrefix := key
			if prefix != "" {
				nextPrefix = prefix + "." + key
			}
			flattenJSONInto(nextPrefix, nested, flat)
		}
	case []any:
		return
	case string:
		flat[canonicalFieldName(prefix)] = strings.TrimSpace(typed)
	case json.Number:
		flat[canonicalFieldName(prefix)] = typed.String()
	case float64:
		flat[canonicalFieldName(prefix)] = strconv.FormatFloat(typed, 'g', -1, 64)
	case bool:
		if typed {
			flat[canonicalFieldName(prefix)] = "true"
		} else {
			flat[canonicalFieldName(prefix)] = "false"
		}
	default:
		if value != nil {
			flat[canonicalFieldName(prefix)] = strings.TrimSpace(fmt.Sprint(value))
		}
	}
}

func decodeJSONValue(blob []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(blob))
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return nil
}

func requireField(record map[string]string, aliases ...string) (string, error) {
	value := firstNonEmptyField(record, aliases...)
	if value == "" {
		return "", fmt.Errorf("missing required field %q", aliases[0])
	}
	return value, nil
}

func parseBillingTimeForField(record map[string]string, aliases ...string) (time.Time, error) {
	value, err := requireField(record, aliases...)
	if err != nil {
		return time.Time{}, err
	}
	return parseBillingTime(value)
}

func parseDecimalField(record map[string]string, aliases ...string) (*big.Rat, error) {
	value, err := requireField(record, aliases...)
	if err != nil {
		return nil, err
	}
	return parseDecimalRat(value)
}

func firstNonEmptyField(record map[string]string, aliases ...string) string {
	for _, alias := range aliases {
		if value := strings.TrimSpace(record[canonicalFieldName(alias)]); value != "" {
			return value
		}
	}
	return ""
}

func canonicalFieldName(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, character := range strings.TrimSpace(value) {
		switch {
		case character >= 'A' && character <= 'Z':
			builder.WriteRune(character + ('a' - 'A'))
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func normalizeImportedChargeKind(value string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	switch {
	case trimmed == "":
		return "", fmt.Errorf("charge kind is required")
	case trimmed == ChargeKindCPU,
		strings.Contains(trimmed, "cpu"),
		strings.Contains(trimmed, "vcpu"),
		strings.Contains(trimmed, "core"):
		return ChargeKindCPU, nil
	case trimmed == ChargeKindMemory,
		strings.Contains(trimmed, "memory"),
		strings.Contains(trimmed, "ram"):
		return ChargeKindMemory, nil
	case trimmed == ChargeKindEphemeralStorage,
		trimmed == "storage",
		strings.Contains(trimmed, "storage"),
		strings.Contains(trimmed, "disk"),
		strings.Contains(trimmed, "ssd"),
		strings.Contains(trimmed, "ephemeral"):
		return ChargeKindEphemeralStorage, nil
	case trimmed == ChargeKindPod,
		strings.Contains(trimmed, "pod"):
		return ChargeKindPod, nil
	case trimmed == ChargeKindRequest,
		strings.Contains(trimmed, "request"),
		strings.Contains(trimmed, "api call"):
		return ChargeKindRequest, nil
	default:
		return "", fmt.Errorf("unsupported charge kind %q", value)
	}
}

func parseDecimalRat(value string) (*big.Rat, error) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(value, "$"))
	if trimmed == "" {
		return nil, fmt.Errorf("decimal value is required")
	}
	rat, ok := new(big.Rat).SetString(trimmed)
	if !ok {
		return nil, fmt.Errorf("invalid decimal value %q", value)
	}
	return rat, nil
}

func decimalRatToRoundedMicros(value *big.Rat) (int64, error) {
	if value == nil {
		return 0, fmt.Errorf("amount is required")
	}
	scaled := new(big.Rat).Mul(value, big.NewRat(1_000_000, 1))
	numerator := new(big.Int).Set(scaled.Num())
	denominator := new(big.Int).Set(scaled.Denom())
	sign := numerator.Sign()
	if sign < 0 {
		numerator.Neg(numerator)
	}
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	remainder.Mul(remainder, big.NewInt(2))
	if remainder.Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if sign < 0 {
		quotient.Neg(quotient)
	}
	if !quotient.IsInt64() {
		return 0, fmt.Errorf("amount overflows int64 micros")
	}
	return quotient.Int64(), nil
}

func parseBillingTime(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, fmt.Errorf("billing time is required")
	}
	if dateOnlyPattern.MatchString(trimmed) {
		return time.ParseInLocation("2006-01-02", trimmed, time.UTC)
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02T15:04:05",
	} {
		if parsedAt, err := time.Parse(layout, trimmed); err == nil {
			return parsedAt.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid billing time %q", value)
}

func gcpBillingPeriod(record map[string]string) (time.Time, time.Time, error) {
	startAt := firstNonEmptyField(record, "billingperiodstartat", "billingperiodstart", "billingperiodstartdate")
	endAt := firstNonEmptyField(record, "billingperiodendat", "billingperiodend", "billingperiodenddate")
	if startAt != "" || endAt != "" {
		parsedStartAt, err := parseBillingTime(startAt)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		parsedEndAt, err := parseBillingTime(endAt)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		return parsedStartAt, parsedEndAt, nil
	}

	invoiceMonth := firstNonEmptyField(record, "invoicemonth")
	if invoiceMonth == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("missing required field %q", "invoice.month")
	}
	month := strings.TrimSpace(invoiceMonth)
	switch {
	case len(month) == 6:
		month = month[:4] + "-" + month[4:]
	case len(month) == 7:
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("invalid invoice month %q", invoiceMonth)
	}
	startAtParsed, err := time.ParseInLocation("2006-01", month, time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid invoice month %q", invoiceMonth)
	}
	return startAtParsed, startAtParsed.AddDate(0, 1, 0), nil
}

func buildAWSResourceKey(record map[string]string, region string, isCUR2 bool) (string, error) {
	tags := awsCURResourceTags(record)
	clusterID := firstNonEmptyField(record, "resourcetagsusersynaraclusterid")
	namespace := firstNonEmptyField(record, "resourcetagsusersynaranamespace")
	podName := firstNonEmptyField(record, "resourcetagsusersynarapod")
	instanceUID := firstNonEmptyField(record, "resourcetagsusersynarainstanceuid")
	if clusterID == "" {
		clusterID = firstNestedMapValue(tags, "usersynaraclusterid")
	}
	if namespace == "" {
		namespace = firstNestedMapValue(tags, "usersynaranamespace")
	}
	if podName == "" {
		podName = firstNestedMapValue(tags, "usersynarapod")
	}
	if instanceUID == "" {
		instanceUID = firstNestedMapValue(tags, "usersynarainstanceuid")
	}
	tagPopulated := countNonEmpty(clusterID, namespace, podName, instanceUID)
	tagExact := tagPopulated == 4
	if tagExact {
		if _, err := uuid.Parse(instanceUID); err != nil {
			return "", fmt.Errorf("trusted Synara instance UID %q is not a UUID", instanceUID)
		}
		if !awsRegionPattern.MatchString(strings.ToLower(strings.TrimSpace(region))) {
			return "", fmt.Errorf("trusted Synara tags require a valid AWS region, got %q", region)
		}
	}
	if !isCUR2 {
		if tagPopulated > 0 && !tagExact {
			return "", fmt.Errorf("partial kubernetes correlation labels are not allowed")
		}
		if tagExact {
			return kubernetesResourceKey("kubernetes", clusterID, region, namespace, podName, instanceUID), nil
		}
	}

	resourceID := firstNonEmptyField(record, "lineitemresourceid")
	arnIdentity, arnKind, arnErr := parseAWSEKSPodIdentity(resourceID)
	if arnErr != nil && tagExact {
		return "", arnErr
	}
	if isCUR2 {
		if arnKind {
			if arnErr == nil {
				accountID := firstNonEmptyField(record, "lineitemusageaccountid")
				if region != "" && !strings.EqualFold(region, arnIdentity.Region) {
					return "", fmt.Errorf("EKS Pod ARN region %q conflicts with row region %q", arnIdentity.Region, region)
				}
				if accountID != "" && accountID != arnIdentity.AccountID {
					return "", fmt.Errorf("EKS Pod ARN account %q conflicts with usage account %q", arnIdentity.AccountID, accountID)
				}
				if (clusterID != "" && clusterID != arnIdentity.ClusterID) ||
					(namespace != "" && namespace != arnIdentity.Namespace) ||
					(podName != "" && podName != arnIdentity.PodName) ||
					(instanceUID != "" && !strings.EqualFold(instanceUID, arnIdentity.PodUID)) {
					return "", errors.New("trusted Synara tags conflict with EKS Pod ARN identity")
				}
				if tagExact {
					return kubernetesResourceKey("kubernetes", clusterID, region, namespace, podName, instanceUID), nil
				}
				return kubernetesResourceKey(
					"kubernetes", arnIdentity.ClusterID, arnIdentity.Region, arnIdentity.Namespace, arnIdentity.PodName, arnIdentity.PodUID,
				), nil
			}
			// A Pod ARN with malformed identity is retained only as an explicitly
			// non-exact provider allocation below.
		}
		if tagExact {
			return kubernetesResourceKey("kubernetes", clusterID, region, namespace, podName, instanceUID), nil
		}
	}
	parentResourceID := firstNonEmptyField(record, "splitlineitemparentresourceid")
	allocationQuality := firstNonEmptyField(record, "synaraallocationquality")
	if allocationQuality == "" {
		allocationQuality = "provider-resource"
	}
	if parentResourceID != "" {
		allocationQuality = "split-missing-pod-uid"
	} else if tagPopulated > 0 {
		allocationQuality = "partial-kubernetes-identity"
	} else if arnKind && arnErr != nil {
		allocationQuality = "invalid-eks-pod-identity"
	}
	if resourceID == "" {
		resourceID = parentResourceID
	}
	if resourceID == "" {
		return "", fmt.Errorf("missing required field %q", "line_item_resource_id")
	}
	accountID := firstNonEmptyField(record, "lineitemusageaccountid")
	if accountID == "" {
		accountID = awsAccountIDFromResourceID(resourceID)
	}
	if !isCUR2 {
		return providerResourceKey("aws", accountID, region, resourceID), nil
	}
	return providerResourceKey("aws-allocation-"+allocationQuality, accountID, region, resourceID), nil
}

func countNonEmpty(values ...string) int {
	count := 0
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			count++
		}
	}
	return count
}

func awsCURResourceTags(record map[string]string) map[string]string {
	raw := firstNonEmptyField(record, "resourcetags")
	if raw == "" {
		return nil
	}
	result := make(map[string]string)
	var jsonTags map[string]any
	if err := json.Unmarshal([]byte(raw), &jsonTags); err == nil {
		for key, value := range jsonTags {
			result[canonicalFieldName(key)] = strings.TrimSpace(fmt.Sprint(value))
		}
		return result
	}
	trimmed := strings.TrimSpace(strings.Trim(raw, "{}"))
	for _, part := range strings.Split(trimmed, ",") {
		key, value, found := strings.Cut(part, "=")
		if !found {
			key, value, found = strings.Cut(part, ":")
		}
		if !found {
			continue
		}
		result[canonicalFieldName(strings.Trim(strings.TrimSpace(key), `"'`))] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return result
}

func firstNestedMapValue(values map[string]string, aliases ...string) string {
	for _, alias := range aliases {
		if value := strings.TrimSpace(values[canonicalFieldName(alias)]); value != "" {
			return value
		}
	}
	return ""
}

type awsEKSPodIdentity struct {
	Region, AccountID, ClusterID, Namespace, PodName, PodUID string
}

func parseAWSEKSPodIdentity(resourceID string) (awsEKSPodIdentity, bool, error) {
	trimmed := strings.TrimSpace(resourceID)
	parts := strings.SplitN(trimmed, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "eks" || !strings.HasPrefix(parts[5], "pod/") {
		return awsEKSPodIdentity{}, false, nil
	}
	resourceParts := strings.Split(strings.TrimPrefix(parts[5], "pod/"), "/")
	if parts[3] == "" || parts[4] == "" || len(resourceParts) != 4 {
		return awsEKSPodIdentity{}, true, errors.New("EKS Pod ARN identity is incomplete")
	}
	if parts[1] != "aws" && parts[1] != "aws-cn" && parts[1] != "aws-us-gov" {
		return awsEKSPodIdentity{}, true, fmt.Errorf("EKS Pod ARN partition %q is unsupported", parts[1])
	}
	if !awsRegionPattern.MatchString(parts[3]) || !awsAccountIDPattern.MatchString(parts[4]) {
		return awsEKSPodIdentity{}, true, errors.New("EKS Pod ARN region or account is invalid")
	}
	for _, value := range resourceParts {
		if strings.TrimSpace(value) == "" {
			return awsEKSPodIdentity{}, true, errors.New("EKS Pod ARN identity is incomplete")
		}
	}
	if _, err := uuid.Parse(resourceParts[3]); err != nil {
		return awsEKSPodIdentity{}, true, fmt.Errorf("EKS Pod ARN UID %q is not a UUID", resourceParts[3])
	}
	return awsEKSPodIdentity{
		Region: parts[3], AccountID: parts[4], ClusterID: resourceParts[0], Namespace: resourceParts[1],
		PodName: resourceParts[2], PodUID: resourceParts[3],
	}, true, nil
}

func buildGCPResourceKey(record map[string]string, region string) (string, error) {
	clusterID, namespace, podName, instanceUID, hasKubernetesLabels, err := kubernetesLabelSet(record,
		[]string{"labelssynaraclusterid", "labelsk8scluster", "labelsclusterid"},
		[]string{"labelssynaranamespace", "labelsk8snamespace", "labelsnamespace"},
		[]string{"labelssynarapod", "labelsk8spod", "labelspodname"},
		[]string{"labelssynarainstanceuid", "labelsk8sinstanceuid", "labelsinstanceuid"},
	)
	if err != nil {
		return "", err
	}
	if hasKubernetesLabels {
		return kubernetesResourceKey("kubernetes", clusterID, region, namespace, podName, instanceUID), nil
	}

	resourceName, err := requireField(record, "resourceglobalname", "resourcename")
	if err != nil {
		return "", err
	}
	projectID := firstNonEmptyField(record, "projectid", "projectname")
	if projectID == "" {
		projectID = gcpProjectIDFromGlobalName(resourceName)
	}
	return providerResourceKey("gcp", projectID, region, resourceName), nil
}

func buildAzureResourceKey(record map[string]string) (string, error) {
	region := firstNonEmptyField(record, "region", "resourcelocation", "location")
	clusterID, namespace, podName, instanceUID, hasKubernetesLabels, err := kubernetesLabelSet(record,
		[]string{"clusterid"},
		[]string{"namespace"},
		[]string{"podname"},
		[]string{"instanceuid"},
	)
	if err != nil {
		return "", err
	}
	if hasKubernetesLabels {
		return kubernetesResourceKey("kubernetes", clusterID, region, namespace, podName, instanceUID), nil
	}

	resourceID, err := requireField(record, "resourceid")
	if err != nil {
		return "", err
	}
	return providerResourceKey("azure", azureSubscriptionID(resourceID), region, resourceID), nil
}

func kubernetesLabelSet(
	record map[string]string,
	clusterAliases []string,
	namespaceAliases []string,
	podAliases []string,
	instanceAliases []string,
) (clusterID string, namespace string, podName string, instanceUID string, hasLabels bool, err error) {
	clusterID = firstNonEmptyField(record, clusterAliases...)
	namespace = firstNonEmptyField(record, namespaceAliases...)
	podName = firstNonEmptyField(record, podAliases...)
	instanceUID = firstNonEmptyField(record, instanceAliases...)
	values := []string{clusterID, namespace, podName, instanceUID}
	populated := 0
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			populated++
		}
	}
	if populated == 0 {
		return "", "", "", "", false, nil
	}
	if populated != len(values) {
		return "", "", "", "", false, fmt.Errorf("partial kubernetes correlation labels are not allowed")
	}
	return clusterID, namespace, podName, instanceUID, true, nil
}

func kubernetesResourceKey(targetKind, clusterID, region, namespace, podName, instanceUID string) string {
	parts := []string{
		normalizeCorrelationPart(targetKind),
		normalizeCorrelationPart(clusterID),
		normalizeCorrelationPart(region),
		normalizeCorrelationPart(namespace),
		normalizeCorrelationPart(podName),
		normalizeCorrelationPart(strings.ToLower(strings.TrimSpace(instanceUID))),
	}
	return strings.Join(parts, ":")
}

func providerResourceKey(provider, accountOrProject, region, resourceID string) string {
	parts := []string{
		normalizeCorrelationPart(strings.ToLower(strings.TrimSpace(provider))),
		normalizeCorrelationPart(strings.ToLower(strings.TrimSpace(accountOrProject))),
		normalizeCorrelationPart(strings.ToLower(strings.TrimSpace(region))),
		normalizeCorrelationPart(strings.ToLower(strings.TrimSpace(resourceID))),
	}
	return strings.Join(parts, ":")
}

func awsRegionFromAZ(value string) string {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if len(trimmed) >= len("us-east-1a") && trimmed[2] == '-' && trimmed[len(trimmed)-1] >= 'a' && trimmed[len(trimmed)-1] <= 'z' {
		return trimmed[:len(trimmed)-1]
	}
	return trimmed
}

func awsRegionFromResourceID(resourceID string) string {
	trimmed := strings.TrimSpace(resourceID)
	if !strings.HasPrefix(trimmed, "arn:") {
		return ""
	}
	parts := strings.Split(trimmed, ":")
	if len(parts) < 4 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(parts[3]))
}

func awsAccountIDFromResourceID(resourceID string) string {
	trimmed := strings.TrimSpace(resourceID)
	if !strings.HasPrefix(trimmed, "arn:") {
		return ""
	}
	parts := strings.Split(trimmed, ":")
	if len(parts) < 5 {
		return ""
	}
	return strings.TrimSpace(parts[4])
}

func gcpLocationFromGlobalName(resourceName string) string {
	trimmed := strings.TrimSpace(resourceName)
	parts := strings.Split(trimmed, "/")
	for index := 0; index < len(parts)-1; index++ {
		if parts[index] == "locations" && index+1 < len(parts) {
			return parts[index+1]
		}
	}
	return ""
}

func gcpProjectIDFromGlobalName(resourceName string) string {
	trimmed := strings.TrimSpace(resourceName)
	parts := strings.Split(trimmed, "/")
	for index := 0; index < len(parts)-1; index++ {
		if parts[index] == "projects" && index+1 < len(parts) {
			return parts[index+1]
		}
	}
	return ""
}

func azureSubscriptionID(resourceID string) string {
	cleaned := strings.Trim(strings.ToLower(strings.TrimSpace(resourceID)), "/")
	parts := strings.Split(cleaned, "/")
	for index := 0; index < len(parts)-1; index++ {
		if parts[index] == "subscriptions" {
			return parts[index+1]
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func isAllEmpty(record map[string]string) bool {
	for _, value := range record {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}

func normalizeProviderResourceID(value string) string {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" {
		return ""
	}
	return strings.ToLower(path.Clean(strings.ReplaceAll(cleaned, "\\", "/")))
}
