package meteringv1_test

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/truongpx396/intel-payment/api/meteringv1"
)

// Money crosses the wire as int64 (constitution I). Exactly one field is floating point: a fraction
// of a limit at which to warn, which is not money. Anything else that is a float or a double is a defect.
var allowedFloats = map[string]bool{"Limit.warn_at": true}

func floatFields(fd protoreflect.FileDescriptor) []string {
	var out []string
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Get(i)
		fields := m.Fields()
		for j := 0; j < fields.Len(); j++ {
			if k := fields.Get(j).Kind(); k == protoreflect.FloatKind || k == protoreflect.DoubleKind {
				out = append(out, string(m.Name())+"."+string(fields.Get(j).Name()))
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestNoMoneyBearingMessageHasAFloatField(t *testing.T) {
	t.Parallel()
	for _, f := range floatFields(meteringv1.File_metering_v1_metering_proto) {
		if !allowedFloats[f] {
			t.Errorf("%s is a float or double on the wire: money is int64 minor units, never a float (the only exception is Limit.warn_at, a fraction)", f)
		}
	}
	// And the allowance is not stale: the field it names still exists.
	found := map[string]bool{}
	for _, f := range floatFields(meteringv1.File_metering_v1_metering_proto) {
		found[f] = true
	}
	for f := range allowedFloats {
		if !found[f] {
			t.Errorf("the allowance for %s is stale: no such float field", f)
		}
	}
}

// The check has teeth: it finds a double planted in a message that carries an amount.
func TestTheFloatScanFindsAPlantedDouble(t *testing.T) {
	t.Parallel()
	double := descriptorpb.FieldDescriptorProto_TYPE_DOUBLE
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: ptr("planted.proto"), Package: ptr("planted"), Syntax: ptr("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: ptr("Price"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: ptr("credits"), Number: ptr(int32(1)), Type: &double, Label: &opt},
			},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := floatFields(fd); len(got) != 1 || got[0] != "Price.credits" {
		t.Fatalf("a double named credits must be found: %v", got)
	}
}

func ptr[T any](v T) *T { return &v }

// The contract's proto block and the file are the same surface, field for field: neither can drift.

var (
	blockRe = regexp.MustCompile("(?s)```proto\n(.*?)```")
	msgRe   = regexp.MustCompile(`(?s)message\s+(\w+)\s*\{(.*?)\}`)
	enumRe  = regexp.MustCompile(`(?s)enum\s+(\w+)\s*\{(.*?)\}`)
	rpcRe   = regexp.MustCompile(`rpc\s+(\w+)\s*\(\s*(\w+)\s*\)\s*returns\s*\(\s*(\w+)\s*\)`)
	fieldRe = regexp.MustCompile(`(repeated\s+)?(map\s*<\s*\w+\s*,\s*\w+\s*>|[\w.]+)\s+(\w+)\s*=\s*(\d+)\s*;`)
	valueRe = regexp.MustCompile(`(\w+)\s*=\s*(\d+)\s*;`)
	lineCmt = regexp.MustCompile(`//[^\n]*`)
	spaces  = regexp.MustCompile(`\s+`)
)

// surfaceOfBlock reads the compact proto in the contract into "Message.field=N:type" entries.
func surfaceOfBlock(text string) []string {
	text = lineCmt.ReplaceAllString(text, "")
	var out []string
	for _, m := range msgRe.FindAllStringSubmatch(text, -1) {
		for _, f := range fieldRe.FindAllStringSubmatch(m[2], -1) {
			typ := spaces.ReplaceAllString(f[2], "")
			if f[1] != "" {
				typ = "repeated " + typ
			}
			out = append(out, fmt.Sprintf("%s.%s=%s:%s", m[1], f[3], f[4], typ))
		}
	}
	for _, e := range enumRe.FindAllStringSubmatch(text, -1) {
		for _, v := range valueRe.FindAllStringSubmatch(e[2], -1) {
			out = append(out, fmt.Sprintf("enum %s.%s=%s", e[1], v[1], v[2]))
		}
	}
	for _, r := range rpcRe.FindAllStringSubmatch(text, -1) {
		out = append(out, fmt.Sprintf("rpc %s(%s)%s", r[1], r[2], r[3]))
	}
	sort.Strings(out)
	return out
}

func typeName(f protoreflect.FieldDescriptor) string {
	one := func(fd protoreflect.FieldDescriptor) string {
		switch fd.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			n := string(fd.Message().FullName())
			return strings.TrimPrefix(n, "metering.v1.")
		case protoreflect.EnumKind:
			return string(fd.Enum().Name())
		default:
			return fd.Kind().String()
		}
	}
	if f.IsMap() {
		return "map<" + one(f.MapKey()) + "," + one(f.MapValue()) + ">"
	}
	if f.IsList() {
		return "repeated " + one(f)
	}
	return one(f)
}

func surfaceOfDescriptor(fd protoreflect.FileDescriptor) []string {
	var out []string
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Get(i)
		if m.IsMapEntry() {
			continue
		}
		fields := m.Fields()
		for j := 0; j < fields.Len(); j++ {
			f := fields.Get(j)
			out = append(out, fmt.Sprintf("%s.%s=%d:%s", m.Name(), f.Name(), f.Number(), typeName(f)))
		}
	}
	enums := fd.Enums()
	for i := 0; i < enums.Len(); i++ {
		e := enums.Get(i)
		for j := 0; j < e.Values().Len(); j++ {
			v := e.Values().Get(j)
			out = append(out, fmt.Sprintf("enum %s.%s=%d", e.Name(), v.Name(), v.Number()))
		}
	}
	svcs := fd.Services()
	for i := 0; i < svcs.Len(); i++ {
		ms := svcs.Get(i).Methods()
		for j := 0; j < ms.Len(); j++ {
			m := ms.Get(j)
			out = append(out, fmt.Sprintf("rpc %s(%s)%s", m.Name(), m.Input().Name(), m.Output().Name()))
		}
	}
	sort.Strings(out)
	return out
}

func diff(a, b []string) (onlyA, onlyB []string) {
	in := func(set []string) map[string]bool {
		m := map[string]bool{}
		for _, s := range set {
			m[s] = true
		}
		return m
	}
	ma, mb := in(a), in(b)
	for _, s := range a {
		if !mb[s] {
			onlyA = append(onlyA, s)
		}
	}
	for _, s := range b {
		if !ma[s] {
			onlyB = append(onlyB, s)
		}
	}
	return
}

func contractBlock(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../specs/001-metering-billing-core/contracts/metering-ports.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(raw), "## Service surface: `MeteringService`")
	if !ok {
		t.Fatal("the contract has no Service surface section")
	}
	m := blockRe.FindStringSubmatch(after)
	if m == nil {
		t.Fatal("the Service surface section has no proto block")
	}
	return m[1]
}

func TestTheContractsProtoBlockIsTheDescriptor(t *testing.T) {
	t.Parallel()
	block := surfaceOfBlock(contractBlock(t))
	desc := surfaceOfDescriptor(meteringv1.File_metering_v1_metering_proto)
	if len(block) < 40 {
		t.Fatalf("the contract's block parsed to only %d entries — is the parser reading it?", len(block))
	}
	onlyBlock, onlyDesc := diff(block, desc)
	if len(onlyBlock)+len(onlyDesc) > 0 {
		t.Fatalf("the contract and api/proto/metering/v1/metering.proto disagree:\n  only in the contract: %v\n  only in the proto:    %v", onlyBlock, onlyDesc)
	}
}

// The comparison has teeth: drop a field from the contract's text and it is reported.
func TestTheContractComparisonFindsADroppedField(t *testing.T) {
	t.Parallel()
	doctored := strings.Replace(contractBlock(t), "int64 writeoff = 8;", "", 1)
	if doctored == contractBlock(t) {
		t.Fatal("the planted edit did not apply: the block has no writeoff field")
	}
	_, onlyDesc := diff(surfaceOfBlock(doctored), surfaceOfDescriptor(meteringv1.File_metering_v1_metering_proto))
	if len(onlyDesc) != 1 || onlyDesc[0] != "Receipt.writeoff=8:int64" {
		t.Fatalf("the missing field must be named: %v", onlyDesc)
	}
}
