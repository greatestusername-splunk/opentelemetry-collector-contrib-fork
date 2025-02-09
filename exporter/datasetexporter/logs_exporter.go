// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package datasetexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/datasetexporter"

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/scalyr/dataset-go/pkg/api/add_events"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
)

var now = time.Now

// If a LogRecord doesn't contain severity or we can't map it to a valid DataSet severity, we use
// this value (3 - INFO) instead
const defaultDataSetSeverityLevel int = dataSetLogLevelInfo

// Constants for valid DataSet log levels (aka Event.sev int field value)
const (
	dataSetLogLevelFinest = 0
	dataSetLogLevelTrace  = 1
	dataSetLogLevelDebug  = 2
	dataSetLogLevelInfo   = 3
	dataSetLogLevelWarn   = 4
	dataSetLogLevelError  = 5
	dataSetLogLevelFatal  = 6
)

func createLogsExporter(ctx context.Context, set exporter.CreateSettings, config component.Config) (exporter.Logs, error) {
	cfg := castConfig(config)
	e, err := newDatasetExporter("logs", cfg, set)
	if err != nil {
		return nil, fmt.Errorf("cannot get DataSetExpoter: %w", err)
	}

	return exporterhelper.NewLogsExporter(
		ctx,
		set,
		config,
		e.consumeLogs,
		exporterhelper.WithQueue(cfg.QueueSettings),
		exporterhelper.WithRetry(cfg.RetrySettings),
		exporterhelper.WithTimeout(cfg.TimeoutSettings),
		exporterhelper.WithShutdown(e.shutdown),
	)
}

func buildBody(attrs map[string]interface{}, value pcommon.Value) string {
	message := value.AsString()
	attrs["body.type"] = value.Type().String()
	switch value.Type() {
	case pcommon.ValueTypeEmpty:
		attrs["body.empty"] = value.AsString()
	case pcommon.ValueTypeStr:
		attrs["body.str"] = value.Str()
	case pcommon.ValueTypeBool:
		attrs["body.bool"] = value.Bool()
	case pcommon.ValueTypeDouble:
		attrs["body.double"] = value.Double()
	case pcommon.ValueTypeInt:
		attrs["body.int"] = value.Int()
	case pcommon.ValueTypeMap:
		updateWithPrefixedValues(attrs, "body.map.", ".", value.Map().AsRaw(), 0)
	case pcommon.ValueTypeBytes:
		attrs["body.bytes"] = value.AsString()
	case pcommon.ValueTypeSlice:
		attrs["body.slice"] = value.AsRaw()
	default:
		attrs["body.unknown"] = value.AsString()
	}

	return message
}

// Function maps OTel severity on the LogRecord to DataSet severity level (number)
func mapOtelSeverityToDataSetSeverity(log plog.LogRecord) int {
	// This function maps OTel severity level to DataSet severity levels
	//
	// Valid OTel levels - https://opentelemetry.io/docs/specs/otel/logs/data-model/#field-severitynumber
	// and valid DataSet ones - https://github.com/scalyr/logstash-output-scalyr/blob/master/lib/logstash/outputs/scalyr.rb#L70
	sevNum := log.SeverityNumber()
	sevText := log.SeverityText()

	dataSetSeverity := defaultDataSetSeverityLevel

	if sevNum > 0 {
		dataSetSeverity = mapLogRecordSevNumToDataSetSeverity(sevNum)
	} else if sevText != "" {
		// Per docs, SeverityNumber is optional so if it's not present we fall back to SeverityText
		// https://opentelemetry.io/docs/specs/otel/logs/data-model/#field-severitytext
		dataSetSeverity = mapLogRecordSeverityTextToDataSetSeverity(sevText)
	}

	return dataSetSeverity
}

func mapLogRecordSevNumToDataSetSeverity(sevNum plog.SeverityNumber) int {
	// Maps LogRecord.SeverityNumber field value to DataSet severity value.
	dataSetSeverity := defaultDataSetSeverityLevel

	if sevNum <= 0 {
		return dataSetSeverity
	}

	// See https://opentelemetry.io/docs/specs/otel/logs/data-model/#field-severitynumber
	// for OTEL mappings
	switch sevNum {
	case 1, 2, 3, 4:
		// TRACE
		dataSetSeverity = dataSetLogLevelTrace
	case 5, 6, 7, 8:
		// DEBUG
		dataSetSeverity = dataSetLogLevelDebug
	case 9, 10, 11, 12:
		// INFO
		dataSetSeverity = dataSetLogLevelInfo
	case 13, 14, 15, 16:
		// WARN
		dataSetSeverity = dataSetLogLevelWarn
	case 17, 18, 19, 20:
		// ERROR
		dataSetSeverity = dataSetLogLevelError
	case 21, 22, 23, 24:
		// FATAL / CRITICAL / EMERGENCY
		dataSetSeverity = dataSetLogLevelFatal
	}

	return dataSetSeverity
}

func mapLogRecordSeverityTextToDataSetSeverity(sevText string) int {
	// Maps LogRecord.SeverityText field value to DataSet severity value.
	dataSetSeverity := defaultDataSetSeverityLevel

	if sevText == "" {
		return dataSetSeverity
	}

	switch strings.ToLower(sevText) {
	case "fine", "finest":
		dataSetSeverity = dataSetLogLevelFinest
	case "trace":
		dataSetSeverity = dataSetLogLevelTrace
	case "debug":
		dataSetSeverity = dataSetLogLevelDebug
	case "info", "information":
		dataSetSeverity = dataSetLogLevelInfo
	case "warn", "warning":
		dataSetSeverity = dataSetLogLevelWarn
	case "error":
		dataSetSeverity = dataSetLogLevelError
	case "fatal", "critical", "emergency":
		dataSetSeverity = dataSetLogLevelFatal
	}

	return dataSetSeverity
}

func buildEventFromLog(
	log plog.LogRecord,
	resource pcommon.Resource,
	scope pcommon.InstrumentationScope,
	settings LogsSettings,
) *add_events.EventBundle {
	attrs := make(map[string]interface{})
	event := add_events.Event{}

	observedTs := log.ObservedTimestamp().AsTime()

	event.Sev = mapOtelSeverityToDataSetSeverity(log)

	if timestamp := log.Timestamp().AsTime(); !timestamp.Equal(time.Unix(0, 0)) {
		event.Ts = strconv.FormatInt(timestamp.UnixNano(), 10)
	}

	if body := log.Body().AsString(); body != "" {
		attrs["message"] = buildBody(attrs, log.Body())
	}
	if dropped := log.DroppedAttributesCount(); dropped > 0 {
		attrs["dropped_attributes_count"] = dropped
	}
	if !observedTs.Equal(time.Unix(0, 0)) {
		attrs["observed.timestamp"] = observedTs.String()
	}
	if span := log.SpanID().String(); span != "" {
		attrs["span_id"] = span
	}

	if trace := log.TraceID().String(); trace != "" {
		attrs["trace_id"] = trace
	}

	// Event needs to always have timestamp set otherwise it will get set to unix epoch start time
	if event.Ts == "" {
		// ObservedTimestamp should always be set, but in case if it's not, we fall back to
		// current time
		if !observedTs.Equal(time.Unix(0, 0)) {
			event.Ts = strconv.FormatInt(observedTs.UnixNano(), 10)
		} else {
			event.Ts = strconv.FormatInt(now().UnixNano(), 10)
		}
	}

	updateWithPrefixedValues(attrs, "attributes.", ".", log.Attributes().AsRaw(), 0)

	if settings.ExportResourceInfo {
		updateWithPrefixedValues(attrs, "resource.attributes.", ".", resource.Attributes().AsRaw(), 0)
	}
	attrs["scope.name"] = scope.Name()
	updateWithPrefixedValues(attrs, "scope.attributes.", ".", scope.Attributes().AsRaw(), 0)

	event.Attrs = attrs
	event.Log = "LL"
	event.Thread = "TL"
	return &add_events.EventBundle{
		Event:  &event,
		Thread: &add_events.Thread{Id: "TL", Name: "logs"},
		Log:    &add_events.Log{Id: "LL", Attrs: map[string]interface{}{}},
	}
}

func (e *DatasetExporter) consumeLogs(_ context.Context, ld plog.Logs) error {
	var events []*add_events.EventBundle

	resourceLogs := ld.ResourceLogs()
	for i := 0; i < resourceLogs.Len(); i++ {
		resource := resourceLogs.At(i).Resource()
		scopeLogs := resourceLogs.At(i).ScopeLogs()
		for j := 0; j < scopeLogs.Len(); j++ {
			scope := scopeLogs.At(j).Scope()
			logRecords := scopeLogs.At(j).LogRecords()
			for k := 0; k < logRecords.Len(); k++ {
				logRecord := logRecords.At(k)
				events = append(events, buildEventFromLog(logRecord, resource, scope, e.exporterCfg.logsSettings))
			}
		}
	}

	return sendBatch(events, e.client)
}
