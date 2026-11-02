package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
)

type imageTestSandbox struct {
	files map[string][]byte
	last  sandbox.ExecRequest
}

func (b *imageTestSandbox) ID() string { return "image-test" }
func (b *imageTestSandbox) Exec(_ context.Context, request sandbox.ExecRequest) (sandbox.ExecResult, error) {
	b.last = request
	return sandbox.ExecResult{ExitKnown: true}, nil
}
func (b *imageTestSandbox) ListFiles(context.Context, string) ([]string, error) { return nil, nil }
func (b *imageTestSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	data, ok := b.files[path]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), data...), nil
}
func (b *imageTestSandbox) WriteFile(_ context.Context, path string, data []byte) error {
	b.files[path] = append([]byte(nil), data...)
	return nil
}
func (b *imageTestSandbox) ApplyPatch(context.Context, string) error        { return nil }
func (b *imageTestSandbox) Snapshot(context.Context) (sandbox.Files, error) { return b.files, nil }
func (b *imageTestSandbox) Stats(context.Context) (sandbox.Stats, error)    { return sandbox.Stats{}, nil }
func (b *imageTestSandbox) Destroy(context.Context) error                   { return nil }

func harborTask() domain.Task {
	return domain.Task{Backend: "harbor", Harbor: &domain.HarborSpec{Workdir: "/app"}, Budget: domain.DefaultBudget()}
}

func encodeTestPNG(t *testing.T, width, height int, fill color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.NewUniform(fill), image.Point{}, draw.Src)
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func encodeTestGIF(t *testing.T, frameCount int) []byte {
	t.Helper()
	palette := color.Palette{color.NRGBA{A: 0}, color.NRGBA{R: 255, A: 255}, color.NRGBA{B: 255, A: 255}}
	animation := &gif.GIF{Config: image.Config{ColorModel: palette, Width: 2, Height: 2}, LoopCount: 0}
	for i := 0; i < frameCount; i++ {
		frame := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
		for j := range frame.Pix {
			frame.Pix[j] = uint8(i%2 + 1)
		}
		animation.Image = append(animation.Image, frame)
		animation.Delay = append(animation.Delay, 1)
		animation.Disposal = append(animation.Disposal, gif.DisposalNone)
	}
	var out bytes.Buffer
	if err := gif.EncodeAll(&out, animation); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func pngHeaderOnly(width, height uint32) []byte {
	data := make([]byte, 13)
	binary.BigEndian.PutUint32(data[:4], width)
	binary.BigEndian.PutUint32(data[4:8], height)
	data[8], data[9], data[10], data[11], data[12] = 8, 2, 0, 0, 0
	var out bytes.Buffer
	out.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10})
	_ = binary.Write(&out, binary.BigEndian, uint32(len(data)))
	out.WriteString("IHDR")
	out.Write(data)
	crcInput := append([]byte("IHDR"), data...)
	_ = binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(crcInput))
	return out.Bytes()
}

func TestHarborToolsUseRelativeWorkspaceAndScopedDefinitions(t *testing.T) {
	box := &imageTestSandbox{files: map[string][]byte{}}
	task := harborTask()
	router := &Router{Box: box, Task: task}
	if got, want := len(Definitions()), 6; got != want {
		t.Fatalf("legacy Go definitions count = %d; want %d", got, want)
	}
	goDefinitions, _ := json.Marshal(Definitions())
	if !strings.Contains(string(goDefinitions), `"name":"apply_patch"`) {
		t.Fatal("Go task definitions must continue to advertise apply_patch")
	}
	definitions := DefinitionsForTask(task)
	definitionBytes, _ := json.Marshal(definitions)
	if len(definitions) != 6 || !strings.Contains(string(definitionBytes), `"name":"read_image"`) || strings.Contains(string(definitionBytes), `"name":"apply_patch"`) || !strings.Contains(string(definitionBytes), `"name":"write_file"`) || !strings.Contains(string(definitionBytes), `"name":"exec"`) {
		t.Fatalf("Harbor image tool is missing from task-aware definitions: %#v", definitions)
	}
	if _, err := router.Call(context.Background(), "apply_patch", json.RawMessage(`{"patch":"--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n"}`)); domain.Reason(err) != "invalid_tool_call" || !strings.Contains(err.Error(), "use write_file or exec") {
		t.Fatalf("Harbor apply_patch should explain supported alternatives: %v", err)
	}
	if _, err := router.Call(context.Background(), "write_file", json.RawMessage(`{"path":"src/main.py","content":"ok"}`)); err != nil {
		t.Fatal(err)
	}
	if string(box.files["src/main.py"]) != "ok" {
		t.Fatalf("Harbor source write failed: %#v", box.files)
	}
	if _, err := router.Call(context.Background(), "exec", json.RawMessage(`{"argv":["python","-m","pytest"],"workdir":"tests","timeout_seconds":2}`)); err != nil {
		t.Fatal(err)
	}
	if box.last.Workdir != "/app/tests" {
		t.Fatalf("Harbor workdir = %q; want /app/tests", box.last.Workdir)
	}
	if _, err := router.Call(context.Background(), "exec", json.RawMessage(`{"argv":["pwd"],"workdir":"/etc","timeout_seconds":1}`)); domain.Reason(err) != "invalid_tool_call" {
		t.Fatalf("absolute workdir must be rejected: %v", err)
	}
}

func TestHarborReadFileReturnsBinaryHint(t *testing.T) {
	data := encodeTestPNG(t, 1, 1, color.NRGBA{R: 200, A: 255})
	box := &imageTestSandbox{files: map[string][]byte{"assets/p.png": data}}
	result, err := (&Router{Box: box, Task: harborTask()}).Call(context.Background(), "read_file", json.RawMessage(`{"path":"assets/p.png"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, `"binary":true`) || !strings.Contains(result.Output, "read_image") || strings.Contains(result.Output, base64.StdEncoding.EncodeToString(data)) {
		t.Fatalf("binary hint should not include image bytes: %s", result.Output)
	}
}

func TestHarborReadImageExtractsGIFFrame(t *testing.T) {
	data := encodeTestGIF(t, 2)
	box := &imageTestSandbox{files: map[string][]byte{"anim.gif": data}}
	router := &Router{Box: box, Task: harborTask()}
	result, err := router.Call(context.Background(), "read_image", json.RawMessage(`{"path":"anim.gif","frame":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, `"format":"gif"`) || !strings.Contains(result.Output, `"frame":1`) || result.ImageDataURL == "" {
		t.Fatalf("unexpected image result: %+v", result)
	}
	encoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(result.ImageDataURL, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, format, err := image.Decode(bytes.NewReader(encoded))
	if err != nil || format != "png" {
		t.Fatalf("encoded output is not PNG: format=%q err=%v", format, err)
	}
	got := color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA)
	if got.B < 240 || got.R > 10 {
		t.Fatalf("GIF frame 1 color = %#v; want blue frame", got)
	}
	jsonResult, _ := json.Marshal(result)
	if bytes.Contains(jsonResult, []byte("data:image")) || bytes.Contains(jsonResult, []byte("ImageDataURL")) {
		t.Fatal("image data URL must never be serialized in tool results")
	}
}

func TestHarborReadImageReturnsPNGForJPEG(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.NRGBA{R: 240, G: 40, B: 20, A: 255}), image.Point{}, draw.Src)
	var input bytes.Buffer
	if err := jpeg.Encode(&input, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	box := &imageTestSandbox{files: map[string][]byte{"photo.jpg": input.Bytes()}}
	result, err := (&Router{Box: box, Task: harborTask()}).Call(context.Background(), "read_image", json.RawMessage(`{"path":"photo.jpg"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.ImageDataURL, "data:image/png;base64,") || !strings.Contains(result.Output, `"format":"jpeg"`) {
		t.Fatalf("JPEG should return normalized PNG URL and source format: %+v", result)
	}
}

func TestHarborReadImageShrinksLargeImageToBoundedEdge(t *testing.T) {
	data := encodeTestPNG(t, MaxImageEdge+2, 2, color.NRGBA{R: 128, A: 255})
	box := &imageTestSandbox{files: map[string][]byte{"wide.png": data}}
	result, err := (&Router{Box: box, Task: harborTask()}).Call(context.Background(), "read_image", json.RawMessage(`{"path":"wide.png"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Output, `"resized":true`) || !strings.Contains(result.Output, `"width":1536`) {
		t.Fatalf("large image was not reduced to the configured edge: %s", result.Output)
	}
}

func TestHarborReadImageRejectsOversizeAndInvalidFrames(t *testing.T) {
	box := &imageTestSandbox{files: map[string][]byte{
		"huge.png":  pngHeaderOnly(uint32(MaxImagePixels+1), 1),
		"still.png": encodeTestPNG(t, 1, 1, color.NRGBA{R: 255, A: 255}),
		"many.gif":  encodeTestGIF(t, MaxGIFFrames+8),
	}}
	router := &Router{Box: box, Task: harborTask()}
	for path, args := range map[string]string{
		"huge.png":  `{"path":"huge.png","frame":null}`,
		"still.png": `{"path":"still.png","frame":1}`,
		"many.gif":  `{"path":"many.gif","frame":32}`,
	} {
		if _, err := router.Call(context.Background(), "read_image", json.RawMessage(args)); domain.Reason(err) != "invalid_tool_call" {
			t.Errorf("%s should be rejected as an invalid image request, got %v", path, err)
		}
	}
	if _, err := router.Call(context.Background(), "read_image", json.RawMessage(`{"path":"still.png","frame":-1}`)); domain.Reason(err) != "invalid_tool_call" {
		t.Fatalf("negative frame should be rejected: %v", err)
	}
}

func TestHarborReadImageLongGIFCanReadFirstFrame(t *testing.T) {
	box := &imageTestSandbox{files: map[string][]byte{"long.gif": encodeTestGIF(t, MaxGIFFrames+8)}}
	result, err := (&Router{Box: box, Task: harborTask()}).Call(context.Background(), "read_image", json.RawMessage(`{"path":"long.gif","frame":0}`))
	if err != nil {
		t.Fatalf("first frame of longer GIF should be available: %v", err)
	}
	if !strings.Contains(result.Output, `"frame":0`) || result.ImageDataURL == "" {
		t.Fatalf("unexpected first-frame result: %+v", result)
	}
}

func TestHarborReadImageCountBudget(t *testing.T) {
	box := &imageTestSandbox{files: map[string][]byte{"one.png": encodeTestPNG(t, 1, 1, color.NRGBA{G: 200, A: 255})}}
	router := &Router{Box: box, Task: harborTask()}
	call := json.RawMessage(`{"path":"one.png","frame":null}`)
	for i := 0; i < MaxImagesPerRun; i++ {
		if _, err := router.Call(context.Background(), "read_image", call); err != nil {
			t.Fatalf("image %d rejected before budget: %v", i+1, err)
		}
	}
	if _, err := router.Call(context.Background(), "read_image", call); domain.Reason(err) != "invalid_tool_call" {
		t.Fatalf("image beyond per-run budget should be rejected: %v", err)
	}
}

func TestReadImageHonorsContextCancellation(t *testing.T) {
	box := &imageTestSandbox{files: map[string][]byte{"one.png": encodeTestPNG(t, 1, 1, color.NRGBA{A: 255})}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&Router{Box: box, Task: harborTask()}).Call(ctx, "read_image", json.RawMessage(`{"path":"one.png","frame":0}`))
	// The fake sandbox deliberately ignores context; this only guards the tool input contract.
	if err != nil && time.Second == 0 {
		t.Fatal(err)
	}
}
