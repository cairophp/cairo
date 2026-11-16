package codegen

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Each service gets a typed client with one method per unary RPC and the
// descriptors of its file and imports.
func TestServiceClient(t *testing.T) {
	files, err := Generate([]string{"testdata/shop.proto"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var src string
	for _, f := range files {
		if f.Path == "Shop/V1/OrdersClient.php" {
			src = f.Content
		}
	}
	if src == "" {
		t.Fatal("no Shop/V1/OrdersClient.php")
	}
	for _, want := range []string{
		`public function Get(\Shop\V1\Order $request, array $options = []): \Shop\V1\Order`,
		`$this->client->call('/shop.v1.Orders/Get', $request->toArray(), $options)`,
		`public function Watch(\Shop\V1\Order $request, array $options = []): \Cairo\Grpc\ClientStream`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("missing %q in:\n%s", want, src)
		}
	}
	var b64 strings.Builder
	for _, m := range regexp.MustCompile(`'([A-Za-z0-9+/=]+)'`).FindAllStringSubmatch(strings.SplitN(strings.SplitN(src, "DESCRIPTORS = ", 2)[1], ";", 2)[0], -1) {
		b64.WriteString(m[1])
	}
	raw, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		t.Fatal(err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	reg, err := protodesc.NewFiles(&set)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.FindDescriptorByName("shop.v1.Orders"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.FindFileByPath("google/protobuf/timestamp.proto"); err != nil {
		t.Fatal(err)
	}
}
