package tools

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/png"
	"math"
	"strings"

	_ "image/jpeg"
)

const (
	MaxImageInputBytes   = 16 << 20
	MaxImagePixels       = 16_000_000
	MaxImagePNGBytes     = 4 << 20
	MaxImageEdge         = 1536
	MaxGIFFrames         = 32
	MaxImagesPerRun      = 8
	MaxImageContextBytes = 16 << 20
	ImageTokenBudget     = 2048
)

var errImageOutputTooLarge = errors.New("encoded image exceeds 4 MiB output limit")

type imageInfo struct {
	Path           string `json:"path,omitempty"`
	Format         string `json:"format"`
	Frame          int    `json:"frame"`
	OriginalWidth  int    `json:"original_width"`
	OriginalHeight int    `json:"original_height"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	Resized        bool   `json:"resized"`
	EncodedBytes   int    `json:"encoded_bytes"`
}

func encodeImagePNG(src []byte, frame *int) ([]byte, imageInfo, error) {
	if len(src) > MaxImageInputBytes {
		return nil, imageInfo{}, fmt.Errorf("image exceeds %d MiB input limit", MaxImageInputBytes>>20)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return nil, imageInfo{}, fmt.Errorf("unsupported or invalid image: %w", err)
	}
	if cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > MaxImagePixels {
		return nil, imageInfo{}, fmt.Errorf("image dimensions exceed %d-pixel limit", MaxImagePixels)
	}
	selectedFrame := 0
	if frame != nil {
		selectedFrame = *frame
	}
	if selectedFrame < 0 {
		return nil, imageInfo{}, errors.New("frame must be a non-negative integer")
	}

	var decoded image.Image
	if format == "gif" {
		if selectedFrame >= MaxGIFFrames {
			return nil, imageInfo{}, fmt.Errorf("GIF frame %d is beyond the %d-frame decode window", selectedFrame, MaxGIFFrames)
		}
		header, err := scanGIF(src, selectedFrame)
		if err != nil {
			return nil, imageInfo{}, err
		}
		if header.width != cfg.Width || header.height != cfg.Height {
			return nil, imageInfo{}, errors.New("GIF screen dimensions are inconsistent")
		}
		if selectedFrame >= header.frames {
			return nil, imageInfo{}, fmt.Errorf("GIF frame %d is out of range", selectedFrame)
		}
		prefix := append([]byte(nil), src[:header.end]...)
		prefix = append(prefix, 0x3b)
		animation, err := gif.DecodeAll(bytes.NewReader(prefix))
		if err != nil {
			return nil, imageInfo{}, fmt.Errorf("decode GIF: %w", err)
		}
		if len(animation.Image) != header.frames || selectedFrame >= len(animation.Image) {
			return nil, imageInfo{}, errors.New("GIF frame count changed during decoding")
		}
		decoded, err = compositeGIFFrame(animation, selectedFrame)
		if err != nil {
			return nil, imageInfo{}, err
		}
	} else {
		if selectedFrame != 0 {
			return nil, imageInfo{}, fmt.Errorf("%s images only support frame 0", strings.ToUpper(format))
		}
		decoded, _, err = image.Decode(bytes.NewReader(src))
		if err != nil {
			return nil, imageInfo{}, fmt.Errorf("decode image: %w", err)
		}
	}

	original := decoded.Bounds().Size()
	if original.X < 1 || original.Y < 1 || int64(original.X)*int64(original.Y) > MaxImagePixels {
		return nil, imageInfo{}, errors.New("decoded image dimensions exceed pixel limit")
	}
	resized := scaleDown(decoded, MaxImageEdge)
	bounds := resized.Bounds().Size()
	var output bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(imageLimitWriter{buffer: &output, limit: MaxImagePNGBytes}, resized); err != nil {
		if errors.Is(err, errImageOutputTooLarge) {
			return nil, imageInfo{}, errImageOutputTooLarge
		}
		return nil, imageInfo{}, fmt.Errorf("encode PNG: %w", err)
	}
	info := imageInfo{Format: format, Frame: selectedFrame, OriginalWidth: original.X, OriginalHeight: original.Y, Width: bounds.X, Height: bounds.Y, Resized: original.X != bounds.X || original.Y != bounds.Y, EncodedBytes: output.Len()}
	return output.Bytes(), info, nil
}

func imageDataURL(pngBytes []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
}

type imageLimitWriter struct {
	buffer *bytes.Buffer
	limit  int
}

func (w imageLimitWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.buffer.Len() {
		return 0, errImageOutputTooLarge
	}
	return w.buffer.Write(p)
}

func scaleDown(src image.Image, maxEdge int) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	longEdge := max(width, height)
	if longEdge <= maxEdge {
		return src
	}
	scale := float64(maxEdge) / float64(longEdge)
	outW := max(1, int(math.Round(float64(width)*scale)))
	outH := max(1, int(math.Round(float64(height)*scale)))
	dst := image.NewNRGBA(image.Rect(0, 0, outW, outH))
	for y := 0; y < outH; y++ {
		sy := float64(bounds.Min.Y) + (float64(y)+0.5)*float64(height)/float64(outH) - 0.5
		for x := 0; x < outW; x++ {
			sx := float64(bounds.Min.X) + (float64(x)+0.5)*float64(width)/float64(outW) - 0.5
			dst.SetNRGBA(x, y, bilinearNRGBA(src, bounds, sx, sy))
		}
	}
	return dst
}

func bilinearNRGBA(src image.Image, bounds image.Rectangle, x, y float64) color.NRGBA {
	x = math.Max(float64(bounds.Min.X), math.Min(float64(bounds.Max.X-1), x))
	y = math.Max(float64(bounds.Min.Y), math.Min(float64(bounds.Max.Y-1), y))
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	x1, y1 := min(x0+1, bounds.Max.X-1), min(y0+1, bounds.Max.Y-1)
	dx, dy := x-float64(x0), y-float64(y0)
	c00 := color.NRGBAModel.Convert(src.At(x0, y0)).(color.NRGBA)
	c10 := color.NRGBAModel.Convert(src.At(x1, y0)).(color.NRGBA)
	c01 := color.NRGBAModel.Convert(src.At(x0, y1)).(color.NRGBA)
	c11 := color.NRGBAModel.Convert(src.At(x1, y1)).(color.NRGBA)
	blend := func(a, b, c, d uint8) uint8 {
		v := (1-dy)*((1-dx)*float64(a)+dx*float64(b)) + dy*((1-dx)*float64(c)+dx*float64(d))
		return uint8(math.Round(v))
	}
	return color.NRGBA{R: blend(c00.R, c10.R, c01.R, c11.R), G: blend(c00.G, c10.G, c01.G, c11.G), B: blend(c00.B, c10.B, c01.B, c11.B), A: blend(c00.A, c10.A, c01.A, c11.A)}
}

type gifScan struct {
	width, height int
	frames        int
	end           int
}

// scanGIF validates only through the requested frame before DecodeAll allocates.
func scanGIF(data []byte, target int) (gifScan, error) {
	if len(data) < 13 || (string(data[:6]) != "GIF87a" && string(data[:6]) != "GIF89a") {
		return gifScan{}, errors.New("invalid GIF header")
	}
	screenW := int(data[6]) | int(data[7])<<8
	screenH := int(data[8]) | int(data[9])<<8
	if screenW < 1 || screenH < 1 || int64(screenW)*int64(screenH) > MaxImagePixels {
		return gifScan{}, fmt.Errorf("GIF screen exceeds %d-pixel limit", MaxImagePixels)
	}
	packed := data[10]
	offset := 13
	if packed&0x80 != 0 {
		offset += 3 * (1 << ((packed & 0x07) + 1))
	}
	if offset > len(data) {
		return gifScan{}, errors.New("truncated GIF global color table")
	}
	frames := 0
	var totalPixels int64
	for offset < len(data) {
		block := data[offset]
		offset++
		switch block {
		case 0x3b:
			return gifScan{}, errors.New("GIF target frame is out of range")
		case 0x21:
			if offset >= len(data) {
				return gifScan{}, errors.New("truncated GIF extension")
			}
			offset++ // extension label
			var err error
			offset, err = skipGIFSubBlocks(data, offset)
			if err != nil {
				return gifScan{}, err
			}
		case 0x2c:
			if offset+9 > len(data) {
				return gifScan{}, errors.New("truncated GIF image descriptor")
			}
			left := int(data[offset]) | int(data[offset+1])<<8
			top := int(data[offset+2]) | int(data[offset+3])<<8
			width := int(data[offset+4]) | int(data[offset+5])<<8
			height := int(data[offset+6]) | int(data[offset+7])<<8
			framePacked := data[offset+8]
			offset += 9
			if width < 1 || height < 1 || left+width > screenW || top+height > screenH {
				return gifScan{}, errors.New("GIF frame rectangle is outside its logical screen")
			}
			frames++
			if frames > MaxGIFFrames {
				return gifScan{}, fmt.Errorf("GIF exceeds %d-frame limit", MaxGIFFrames)
			}
			totalPixels += int64(width) * int64(height)
			if totalPixels > MaxImagePixels {
				return gifScan{}, fmt.Errorf("GIF frame pixels exceed %d-pixel limit", MaxImagePixels)
			}
			if framePacked&0x80 != 0 {
				offset += 3 * (1 << ((framePacked & 0x07) + 1))
			}
			if offset >= len(data) {
				return gifScan{}, errors.New("truncated GIF image data")
			}
			offset++ // LZW minimum code size
			var err error
			offset, err = skipGIFSubBlocks(data, offset)
			if err != nil {
				return gifScan{}, err
			}
			if frames == target+1 {
				return gifScan{width: screenW, height: screenH, frames: frames, end: offset}, nil
			}
		default:
			return gifScan{}, errors.New("unknown GIF block marker")
		}
	}
	return gifScan{}, errors.New("GIF is missing requested frame")
}

func skipGIFSubBlocks(data []byte, offset int) (int, error) {
	for {
		if offset >= len(data) {
			return 0, errors.New("truncated GIF data sub-blocks")
		}
		size := int(data[offset])
		offset++
		if size == 0 {
			return offset, nil
		}
		if size > len(data)-offset {
			return 0, errors.New("truncated GIF data sub-block")
		}
		offset += size
	}
}

func compositeGIFFrame(animation *gif.GIF, target int) (image.Image, error) {
	width, height := animation.Config.Width, animation.Config.Height
	if width < 1 || height < 1 || int64(width)*int64(height) > MaxImagePixels {
		return nil, errors.New("GIF logical screen exceeds pixel limit")
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, width, height))
	background := color.NRGBA{}
	if palette, ok := animation.Config.ColorModel.(color.Palette); ok && int(animation.BackgroundIndex) < len(palette) {
		background = color.NRGBAModel.Convert(palette[animation.BackgroundIndex]).(color.NRGBA)
	}
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(background), image.Point{}, draw.Src)
	var previousRect image.Rectangle
	var previousDisposal byte
	var saved *image.NRGBA
	for i, frame := range animation.Image {
		if i > target {
			break
		}
		if i > 0 {
			switch previousDisposal {
			case gif.DisposalBackground:
				draw.Draw(canvas, previousRect, image.NewUniform(background), image.Point{}, draw.Src)
			case gif.DisposalPrevious:
				if saved != nil {
					draw.Draw(canvas, canvas.Bounds(), saved, image.Point{}, draw.Src)
				}
			}
		}
		if frame == nil || !frame.Bounds().In(canvas.Bounds()) {
			return nil, errors.New("decoded GIF frame is outside its logical screen")
		}
		currentDisposal := byte(gif.DisposalNone)
		if i < len(animation.Disposal) {
			currentDisposal = animation.Disposal[i]
		}
		saved = nil
		if currentDisposal == gif.DisposalPrevious {
			copyCanvas := image.NewNRGBA(canvas.Bounds())
			draw.Draw(copyCanvas, copyCanvas.Bounds(), canvas, canvas.Bounds().Min, draw.Src)
			saved = copyCanvas
		}
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)
		if i == target {
			return canvas, nil
		}
		previousRect = frame.Bounds()
		previousDisposal = currentDisposal
	}
	return nil, errors.New("GIF target frame is unavailable")
}
