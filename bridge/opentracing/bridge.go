// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opentracing // import "go.opentelemetry.io/otel/bridge/opentracing"

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"

	ot "github.com/opentracing/opentracing-go"
	otext "github.com/opentracing/opentracing-go/ext"
	otlog "github.com/opentracing/opentracing-go/log"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/bridge/opentracing/migration"
	"go.opentelemetry.io/otel/codes"
	iBaggage "go.opentelemetry.io/otel/internal/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

var (
	noopTracer = noop.NewTracerProvider().Tracer("")
	noopSpan   = func() trace.Span {
		_, s := noopTracer.Start(context.Background(), "")
		return s
	}()
)

type bridgeSpanContext struct {
	bag baggage.Baggage
	trace.SpanContext
}

var _ ot.SpanContext = &bridgeSpanContext{}

func newBridgeSpanContext(otelSpanContext trace.SpanContext, parentOtSpanContext ot.SpanContext) *bridgeSpanContext {
	bCtx := &bridgeSpanContext{
		bag:         baggage.Baggage{},
		SpanContext: otelSpanContext,
	}
	if parentOtSpanContext != nil {
		parentOtSpanContext.ForeachBaggageItem(func(key, value string) bool {
			bCtx.setBaggageItem(key, value)
			return true
		})
	}
	return bCtx
}

func (c *bridgeSpanContext) ForeachBaggageItem(handler func(k, v string) bool) {
	for _, m := range c.bag.Members() {
		if !handler(m.Key(), m.Value()) {
			return
		}
	}
}

func (c *bridgeSpanContext) setBaggageItem(restrictedKey, value string) {
	m, err := baggage.NewMemberRaw(restrictedKey, value)
	if err != nil {
		return
	}
	c.bag, _ = c.bag.SetMember(m)
}

func (c *bridgeSpanContext) baggageItem(restrictedKey string) baggage.Member {
	return c.bag.Member(restrictedKey)
}

type bridgeSpan struct {
	otelSpan          trace.Span
	ctx               *bridgeSpanContext
	tracer            *BridgeTracer
	skipDeferHook     bool
	extraBaggageItems map[string]string
}

var _ ot.Span = &bridgeSpan{}

func newBridgeSpan(otelSpan trace.Span, bridgeSC *bridgeSpanContext, tracer *BridgeTracer) *bridgeSpan {
	return &bridgeSpan{
		otelSpan:          otelSpan,
		ctx:               bridgeSC,
		tracer:            tracer,
		skipDeferHook:     false,
		extraBaggageItems: nil,
	}
}

func (s *bridgeSpan) Finish() {
	s.otelSpan.End()
}

func (s *bridgeSpan) FinishWithOptions(opts ot.FinishOptions) {
	var otelOpts []trace.SpanEndOption

	if !opts.FinishTime.IsZero() {
		otelOpts = append(otelOpts, trace.WithTimestamp(opts.FinishTime))
	}
	for _, record := range opts.LogRecords {
		s.logRecord(record)
	}
	for _, data := range opts.BulkLogData {
		s.logRecord(data.ToLogRecord())
	}
	s.otelSpan.End(otelOpts...)
}

func (s *bridgeSpan) logRecord(record ot.LogRecord) {
	s.otelSpan.AddEvent(
		"",
		trace.WithTimestamp(record.Timestamp),
		trace.WithAttributes(otLogFieldsToOTelAttrs(record.Fields)...),
	)
}

func (s *bridgeSpan) Context() ot.SpanContext {
	return s.ctx
}

func (s *bridgeSpan) SetOperationName(operationName string) ot.Span {
	s.otelSpan.SetName(operationName)
	return s
}

// SetTag method adds a tag to the span.
//
// Note about the following value conversions:
// - int -> int64
// - uint -> string
// - int32 -> int64
// - uint32 -> int64
// - uint64 -> string
// - float32 -> float64
func (s *bridgeSpan) SetTag(key string, value any) ot.Span {
	switch key {
	case string(otext.SpanKind):
		// TODO: Should we ignore it?
	case string(otext.Error):
		if b, ok := value.(bool); ok && b {
			s.otelSpan.SetStatus(codes.Error, "")
		}
	default:
		s.otelSpan.SetAttributes(otTagToOTelAttr(key, value))
	}
	return s
}

func (s *bridgeSpan) LogFields(fields ...otlog.Field) {
	s.otelSpan.AddEvent(
		"",
		trace.WithAttributes(otLogFieldsToOTelAttrs(fields)...),
	)
}

type bridgeFieldEncoder struct {
	pairs []attribute.KeyValue
}

var _ otlog.Encoder = &bridgeFieldEncoder{}

func (e *bridgeFieldEncoder) EmitString(key, value string) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitBool(key string, value bool) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitInt(key string, value int) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitInt32(key string, value int32) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitInt64(key string, value int64) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitUint32(key string, value uint32) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitUint64(key string, value uint64) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitFloat32(key string, value float32) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitFloat64(key string, value float64) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitObject(key string, value any) {
	e.emitCommon(key, value)
}

func (e *bridgeFieldEncoder) EmitLazyLogger(value otlog.LazyLogger) {
	value(e)
}

func (e *bridgeFieldEncoder) emitCommon(key string, value any) {
	e.pairs = append(e.pairs, otTagToOTelAttr(key, value))
}

func otLogFieldsToOTelAttrs(fields []otlog.Field) []attribute.KeyValue {
	encoder := &bridgeFieldEncoder{}
	for _, field := range fields {
		field.Marshal(encoder)
	}
	return encoder.pairs
}

func (s *bridgeSpan) LogKV(alternatingKeyValues ...any) {
	fields, err := otlog.InterleavedKVToFields(alternatingKeyValues...)
	if err != nil {
		return
	}
	s.LogFields(fields...)
}

func (s *bridgeSpan) SetBaggageItem(restrictedKey, value string) ot.Span {
	s.updateOTelContext(restrictedKey, value)
	s.setBaggageItemOnly(restrictedKey, value)
	return s
}

func (s *bridgeSpan) setBaggageItemOnly(restrictedKey, value string) {
	s.ctx.setBaggageItem(restrictedKey, value)
}

func (s *bridgeSpan) updateOTelContext(restrictedKey, value string) {
	if s.extraBaggageItems == nil {
		s.extraBaggageItems = make(map[string]string)
	}
	s.extraBaggageItems[restrictedKey] = value
}

func (s *bridgeSpan) BaggageItem(restrictedKey string) string {
	return s.ctx.baggageItem(restrictedKey).Value()
}

func (s *bridgeSpan) Tracer() ot.Tracer {
	return s.tracer
}

func (s *bridgeSpan) LogEvent(event string) {
	s.LogEventWithPayload(event, nil)
}

func (s *bridgeSpan) LogEventWithPayload(event string, payload any) {
	data := ot.LogData{
		Event:   event,
		Payload: payload,
	}
	s.Log(data)
}

func (s *bridgeSpan) Log(data ot.LogData) {
	record := data.ToLogRecord()
	s.LogFields(record.Fields...)
}

type bridgeSetTracer struct {
	isSet      bool
	otelTracer trace.Tracer

	warningHandler BridgeWarningHandler
	warnOnce       sync.Once
}

func (s *bridgeSetTracer) tracer() trace.Tracer {
	if !s.isSet {
		s.warnOnce.Do(func() {
			s.warningHandler(
				"The OpenTelemetry tracer is not set, default no-op tracer is used! Call SetOpenTelemetryTracer to set it up.\n",
			)
		})
	}
	return s.otelTracer
}

// BridgeWarningHandler is a type of handler that receives warnings
// from the BridgeTracer.
type BridgeWarningHandler func(msg string)

// BridgeTracer is an implementation of the OpenTracing tracer, which
// translates the calls to the OpenTracing API into OpenTelemetry
// counterparts and calls the underlying OpenTelemetry tracer.
type BridgeTracer struct {
	setTracer bridgeSetTracer

	warningHandler BridgeWarningHandler
	warnOnce       sync.Once

	propagator propagation.TextMapPropagator
}

var (
	_ ot.Tracer                         = &BridgeTracer{}
	_ ot.TracerContextWithSpanExtension = &BridgeTracer{}
)

// NewBridgeTracer creates a new BridgeTracer. The new tracer forwards
// the calls to the OpenTelemetry Noop tracer, so it should be
// overridden with the SetOpenTelemetryTracer function. The warnings
// handler does nothing by default, so to override it use the
// SetWarningHandler function.
func NewBridgeTracer() *BridgeTracer {
	return &BridgeTracer{
		setTracer: bridgeSetTracer{
			warningHandler: func(msg string) {},
			otelTracer:     noopTracer,
		},
		warningHandler: func(msg string) {},
		propagator:     nil,
	}
}

// SetWarningHandler overrides the warning handler.
func (t *BridgeTracer) SetWarningHandler(handler BridgeWarningHandler) {
	t.setTracer.warningHandler = handler
	t.warningHandler = handler
}

// SetOpenTelemetryTracer overrides the underlying OpenTelemetry
// tracer. The passed tracer should know how to operate in the
// environment that uses OpenTracing API.
func (t *BridgeTracer) SetOpenTelemetryTracer(tracer trace.Tracer) {
	t.setTracer.otelTracer = tracer
	t.setTracer.isSet = true
}

// SetTextMapPropagator sets propagator as the TextMapPropagator to use by the
// BridgeTracer.
func (t *BridgeTracer) SetTextMapPropagator(propagator propagation.TextMapPropagator) {
	t.propagator = propagator
}

// NewHookedContext returns a Context that has ctx as its parent and is
// wrapped to handle baggage set and get operations.
func (t *BridgeTracer) NewHookedContext(ctx context.Context) context.Context {
	ctx = iBaggage.ContextWithSetHook(ctx, t.baggageSetHook)
	ctx = iBaggage.ContextWithGetHook(ctx, t.baggageGetHook)
	return ctx
}

func (t *BridgeTracer) baggageSetHook(ctx context.Context, list iBaggage.List) context.Context {
	span := ot.SpanFromContext(ctx)
	if span == nil {
		t.warningHandler("No active OpenTracing span, can not propagate the baggage items from OpenTelemetry context\n")
		return ctx
	}
	bSpan, ok := span.(*bridgeSpan)
	if !ok {
		t.warningHandler(
			"Encountered a foreign OpenTracing span, will not propagate the baggage items from OpenTelemetry context\n",
		)
		return ctx
	}
	for k, v := range list {
		bSpan.setBaggageItemOnly(k, v.Value)
	}
	return ctx
}

func (t *BridgeTracer) baggageGetHook(ctx context.Context, list iBaggage.List) iBaggage.List {
	span := ot.SpanFromContext(ctx)
	if span == nil {
		t.warningHandler(
			"No active OpenTracing span, can not propagate the baggage items from OpenTracing span context\n",
		)
		return list
	}
	bSpan, ok := span.(*bridgeSpan)
	if !ok {
		t.warningHandler(
			"Encountered a foreign OpenTracing span, will not propagate the baggage items from OpenTracing span context\n",
		)
		return list
	}
	items := bSpan.extraBaggageItems
	if len(items) == 0 {
		return list
	}

	// Privilege of using the internal representation of Baggage here comes
	// with the responsibility to make sure we maintain its immutability. We
	// need to return a copy to ensure this.

	merged := make(iBaggage.List, len(list))
	maps.Copy(merged, list)

	for k, v := range items {
		// Overwrite according to OpenTelemetry specification.
		merged[k] = iBaggage.Item{Value: v}
	}

	return merged
}

// StartSpan is a part of the implementation of the OpenTracing Tracer
// interface.
func (t *BridgeTracer) StartSpan(operationName string, opts ...ot.StartSpanOption) ot.Span {
	sso := ot.StartSpanOptions{}
	for _, opt := range opts {
		opt.Apply(&sso)
	}
	parentBridgeSC, links := otSpanReferencesToParentAndLinks(sso.References)
	attributes, kind, hadTrueErrorTag := otTagsToOTelAttributesKindAndError(sso.Tags)
	checkCtx := migration.WithDeferredSetup(context.Background())
	if parentBridgeSC != nil {
		checkCtx = trace.ContextWithRemoteSpanContext(checkCtx, parentBridgeSC.SpanContext)
	}
	checkCtx2, otelSpan := t.setTracer.tracer().Start(
		checkCtx,
		operationName,
		trace.WithAttributes(attributes...),
		trace.WithTimestamp(sso.StartTime),
		trace.WithLinks(links...),
		trace.WithSpanKind(kind),
	)
	if ot.SpanFromContext(checkCtx2) != nil {
		t.warnOnce.Do(func() {
			t.warningHandler(
				"SDK should have deferred the context setup, see the documentation of go.opentelemetry.io/otel/bridge/opentracing/migration\n",
			)
		})
	}
	if hadTrueErrorTag {
		otelSpan.SetStatus(codes.Error, "")
	}
	// One does not simply pass a concrete pointer to function
	// that takes some interface. In case of passing nil concrete
	// pointer, we get an interface with non-nil type (because the
	// pointer type is known) and a nil value. Which means
	// interface is not nil, but calling some interface function
	// on it will most likely result in nil pointer dereference.
	var otSpanContext ot.SpanContext
	if parentBridgeSC != nil {
		otSpanContext = parentBridgeSC
	}
	sctx := newBridgeSpanContext(otelSpan.SpanContext(), otSpanContext)
	span := newBridgeSpan(otelSpan, sctx, t)

	return span
}

// ContextWithBridgeSpan sets up the context with the passed
// OpenTelemetry span as the active OpenTracing span.
//
// This function should be used by the OpenTelemetry tracers that want
// to be aware how to operate in the environment using OpenTracing
// API.
func (t *BridgeTracer) ContextWithBridgeSpan(ctx context.Context, span trace.Span) context.Context {
	var otSpanContext ot.SpanContext
	if parentSpan := ot.SpanFromContext(ctx); parentSpan != nil {
		otSpanContext = parentSpan.Context()
	}
	bCtx := newBridgeSpanContext(span.SpanContext(), otSpanContext)
	bSpan := newBridgeSpan(span, bCtx, t)
	bSpan.skipDeferHook = true
	return ot.ContextWithSpan(ctx, bSpan)
}

// ContextWithSpanHook is an implementation of the OpenTracing tracer
// extension interface. It will call the DeferredContextSetupHook
// function on the tracer if it implements the
// DeferredContextSetupTracerExtension interface.
func (t *BridgeTracer) ContextWithSpanHook(ctx context.Context, span ot.Span) context.Context {
	bSpan, ok := span.(*bridgeSpan)
	if !ok {
		t.warningHandler(
			"Encountered a foreign OpenTracing span, will not run a possible deferred context setup hook\n",
		)
		return ctx
	}
	if bSpan.skipDeferHook {
		return ctx
	}
	if tracerWithExtension, ok := bSpan.tracer.setTracer.tracer().(migration.DeferredContextSetupTracerExtension); ok {
		ctx = tracerWithExtension.DeferredContextSetupHook(ctx, bSpan.otelSpan)
	}
	return ctx
}

func otTagsToOTelAttributesKindAndError(tags map[string]any) ([]attribute.KeyValue, trace.SpanKind, bool) {
	kind := trace.SpanKindInternal
	err := false
	var pairs []attribute.KeyValue
	for k, v := range tags {
		switch k {
		case string(otext.SpanKind):
			sk := v
			if s, ok := v.(string); ok {
				sk = otext.SpanKindEnum(strings.ToLower(s))
			}
			switch sk {
			case otext.SpanKindRPCClientEnum:
				kind = trace.SpanKindClient
			case otext.SpanKindRPCServerEnum:
				kind = trace.SpanKindServer
			case otext.SpanKindProducerEnum:
				kind = trace.SpanKindProducer
			case otext.SpanKindConsumerEnum:
				kind = trace.SpanKindConsumer
			}
		case string(otext.Error):
			if b, ok := v.(bool); ok && b {
				err = true
			}
		default:
			pairs = append(pairs, otTagToOTelAttr(k, v))
		}
	}
	return pairs, kind, err
}

// otTagToOTelAttr converts given key-value into attribute.KeyValue.
// Note that some conversions are not obvious:
// - int -> int64
// - uint -> string
// - int32 -> int64
// - uint32 -> int64
// - uint64 -> string
// - float32 -> float64
func otTagToOTelAttr(k string, v any) attribute.KeyValue {
	key := otTagToOTelAttrKey(k)
	switch val := v.(type) {
	case bool:
		return key.Bool(val)
	case int64:
		return key.Int64(val)
	case uint64:
		return key.String(strconv.FormatUint(val, 10))
	case float64:
		return key.Float64(val)
	case int8:
		return key.Int64(int64(val))
	case uint8:
		return key.Int64(int64(val))
	case int16:
		return key.Int64(int64(val))
	case uint16:
		return key.Int64(int64(val))
	case int32:
		return key.Int64(int64(val))
	case uint32:
		return key.Int64(int64(val))
	case float32:
		return key.Float64(float64(val))
	case int:
		return key.Int(val)
	case uint:
		return key.String(strconv.FormatUint(uint64(val), 10))
	case string:
		return key.String(val)
	default:
		return key.String(fmt.Sprint(v))
	}
}

func otTagToOTelAttrKey(k string) attribute.Key {
	return attribute.Key(k)
}

func otSpanReferencesToParentAndLinks(references []ot.SpanReference) (*bridgeSpanContext, []trace.Link) {
	var (
		parent *bridgeSpanContext
		links  []trace.Link
	)
	for _, reference := range references {
		bridgeSC, ok := reference.ReferencedContext.(*bridgeSpanContext)
		if !ok {
			// We ignore foreign ot span contexts,
			// sorry. We have no way of getting any
			// TraceID and SpanID out of it for form a
			// OTel SpanContext for OTel Link. And
			// we can't make it a parent - it also needs a
			// valid OTel SpanContext.
			continue
		}
		if parent != nil {
			links = append(links, otSpanReferenceToOTelLink(bridgeSC, reference.Type))
		} else {
			if reference.Type == ot.ChildOfRef {
				parent = bridgeSC
			} else {
				links = append(links, otSpanReferenceToOTelLink(bridgeSC, reference.Type))
			}
		}
	}
	return parent, links
}

func otSpanReferenceToOTelLink(bridgeSC *bridgeSpanContext, refType ot.SpanReferenceType) trace.Link {
	return trace.Link{
		SpanContext: bridgeSC.SpanContext,
		Attributes:  otSpanReferenceTypeToOTelLinkAttributes(refType),
	}
}

func otSpanReferenceTypeToOTelLinkAttributes(refType ot.SpanReferenceType) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("ot-span-reference-type", otSpanReferenceTypeToString(refType)),
	}
}

func otSpanReferenceTypeToString(refType ot.SpanReferenceType) string {
	switch refType {
	case ot.ChildOfRef:
		// "extra", because first child-of reference is used
		// as a parent, so this function isn't even called for
		// it.
		return "extra-child-of"
	case ot.FollowsFromRef:
		return "follows-from-ref"
	default:
		return fmt.Sprintf("unknown-%d", int(refType))
	}
}

// fakeSpan is just a holder of span context, nothing more. It's for
// propagators, so they can get the span context from Go context.
type fakeSpan struct {
	trace.Span
	sc trace.SpanContext
}

func (s fakeSpan) SpanContext() trace.SpanContext {
	return s.sc
}

// Inject is a part of the implementation of the OpenTracing Tracer
// interface.
//
// Currently only the HTTPHeaders and TextMap formats are supported.
func (t *BridgeTracer) Inject(sm ot.SpanContext, format any, carrier any) error {
	bridgeSC, ok := sm.(*bridgeSpanContext)
	if !ok {
		return ot.ErrInvalidSpanContext
	}
	if !bridgeSC.IsValid() {
		return ot.ErrInvalidSpanContext
	}

	builtinFormat, ok := format.(ot.BuiltinFormat)
	if !ok {
		return ot.ErrUnsupportedFormat
	}

	var textCarrier propagation.TextMapCarrier
	var err error

	switch builtinFormat {
	case ot.HTTPHeaders:
		if hhcarrier, ok := carrier.(ot.HTTPHeadersCarrier); ok {
			textCarrier = propagation.HeaderCarrier(hhcarrier)
		} else {
			textCarrier, err = newTextMapWrapperForInject(carrier)
		}
	case ot.TextMap:
		if textCarrier, ok = carrier.(propagation.TextMapCarrier); !ok {
			textCarrier, err = newTextMapWrapperForInject(carrier)
		}
	default:
		err = ot.ErrUnsupportedFormat
	}
	if err != nil {
		return err
	}

	fs := fakeSpan{
		Span: noopSpan,
		sc:   bridgeSC.SpanContext,
	}
	ctx := trace.ContextWithSpan(context.Background(), fs)
	ctx = baggage.ContextWithBaggage(ctx, bridgeSC.bag)
	t.getPropagator().Inject(ctx, textCarrier)
	return nil
}

// Extract is a part of the implementation of the OpenTracing Tracer
// interface.
//
// Currently only the HTTPHeaders and TextMap formats are supported.
func (t *BridgeTracer) Extract(format any, carrier any) (ot.SpanContext, error) {
	builtinFormat, ok := format.(ot.BuiltinFormat)
	if !ok {
		return nil, ot.ErrUnsupportedFormat
	}

	var textCarrier propagation.TextMapCarrier
	var err error

	switch builtinFormat {
	case ot.HTTPHeaders:
		if hhcarrier, ok := carrier.(ot.HTTPHeadersCarrier); ok {
			textCarrier = propagation.HeaderCarrier(hhcarrier)
		} else {
			textCarrier, err = newTextMapWrapperForExtract(carrier)
		}
	case ot.TextMap:
		if textCarrier, ok = carrier.(propagation.TextMapCarrier); !ok {
			textCarrier, err = newTextMapWrapperForExtract(carrier)
		}
	default:
		err = ot.ErrUnsupportedFormat
	}
	if err != nil {
		return nil, err
	}

	ctx := t.getPropagator().Extract(context.Background(), textCarrier)
	bag := baggage.FromContext(ctx)
	bridgeSC := &bridgeSpanContext{
		bag:         bag,
		SpanContext: trace.SpanContextFromContext(ctx),
	}
	if !bridgeSC.IsValid() {
		return nil, ot.ErrSpanContextNotFound
	}
	return bridgeSC, nil
}

func (t *BridgeTracer) getPropagator() propagation.TextMapPropagator {
	if t.propagator != nil {
		return t.propagator
	}
	return otel.GetTextMapPropagator()
}

// textMapWrapper Provides operating.TextMapWriter and operating.TextMapReader to
// propagation.TextMapCarrier compatibility.
// Usually, Inject method will only use the write-related interface.
// Extract method will only use the reade-related interface.
// To avoid panic,
// when the carrier implements only one of the interfaces,
// it provides a default implementation of the other interface (textMapWriter and textMapReader).
type textMapWrapper struct {
	ot.TextMapWriter
	ot.TextMapReader
	readerMap map[string]string
}

func (t *textMapWrapper) Get(key string) string {
	if t.readerMap == nil {
		t.loadMap()
	}

	return t.readerMap[key]
}

func (t *textMapWrapper) Set(key string, value string) {
	t.TextMapWriter.Set(key, value)
}

func (t *textMapWrapper) Keys() []string {
	if t.readerMap == nil {
		t.loadMap()
	}

	str := make([]string, 0, len(t.readerMap))
	for key := range t.readerMap {
		str = append(str, key)
	}

	return str
}

func (t *textMapWrapper) loadMap() {
	t.readerMap = make(map[string]string)

	_ = t.ForeachKey(func(key, val string) error {
		t.readerMap[key] = val

		return nil
	})
}

func newTextMapWrapperForExtract(carrier any) (*textMapWrapper, error) {
	t := &textMapWrapper{}

	reader, ok := carrier.(ot.TextMapReader)
	if !ok {
		return nil, ot.ErrInvalidCarrier
	}

	t.TextMapReader = reader

	writer, ok := carrier.(ot.TextMapWriter)
	if ok {
		t.TextMapWriter = writer
	} else {
		t.TextMapWriter = &textMapWriter{}
	}

	return t, nil
}

func newTextMapWrapperForInject(carrier any) (*textMapWrapper, error) {
	t := &textMapWrapper{}

	writer, ok := carrier.(ot.TextMapWriter)
	if !ok {
		return nil, ot.ErrInvalidCarrier
	}

	t.TextMapWriter = writer

	reader, ok := carrier.(ot.TextMapReader)
	if ok {
		t.TextMapReader = reader
	} else {
		t.TextMapReader = &textMapReader{}
	}

	return t, nil
}

type textMapWriter struct{}

func (t *textMapWriter) Set(key string, value string) {
	// maybe print a warning log.
}

type textMapReader struct{}

func (t *textMapReader) ForeachKey(handler func(key, val string) error) error {
	return nil // maybe print a warning log.
}
