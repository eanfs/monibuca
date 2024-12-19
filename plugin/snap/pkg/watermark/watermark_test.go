package watermark

import (
	"image/color"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/disintegration/imaging"
	"github.com/golang/freetype/truetype"
	"github.com/stretchr/testify/assert"
	xfont "golang.org/x/image/font"
)

func loadTestFont(t *testing.T) *truetype.Font {
	var fontPath string
	switch runtime.GOOS {
	case "windows":
		fontPath = "C:\\Windows\\Fonts\\simsun.ttc"
	case "darwin":
		fontPath = "/System/Library/Fonts/STHeiti Light.ttc"
	case "linux":
		fontPath = "/usr/share/fonts/truetype/winfonts/simsun.ttc"
	default:
		t.Fatal("不支持的操作系统")
	}

	fontBytes, err := os.ReadFile(fontPath)
	if err != nil {
		t.Fatalf("读取字体文件失败: %v", err)
	}

	font, err := truetype.Parse(fontBytes)
	if err != nil {
		t.Fatalf("解析字体失败: %v", err)
	}

	return font
}

func TestDrawWatermarkSingle(t *testing.T) {
	// 准备测试图片
	tmpDir := t.TempDir()
	testImagePath := filepath.Join(tmpDir, "test_input.png")

	// 创建一个测试图片
	img := imaging.New(800, 600, color.White)
	err := imaging.Save(img, testImagePath)
	if err != nil {
		t.Fatalf("创建测试图片失败: %v", err)
	}

	// 加载测试图片
	baseImg, err := imaging.Open(testImagePath)
	if err != nil {
		t.Fatalf("加载测试图片失败: %v", err)
	}

	// 加载字体
	font := loadTestFont(t)

	// 创建水印配置
	config := TextConfig{
		Text:       "测试水印",
		Font:       font,
		FontSize:   36,
		Spacing:    10,
		RowSpacing: 10,
		ColSpacing: 20,
		DPI:        72,
		Color:      color.RGBA{R: 255, G: 0, B: 0, A: 255}, // 红色
		IsGrid:     false,
		Angle:      45,
		OffsetX:    100,
		OffsetY:    100,
	}

	// 测试单个水印
	result, err := DrawWatermarkSingle(baseImg, config, true)
	if err != nil {
		t.Fatalf("绘制单个水印失败: %v", err)
	}

	// 保存结果用于目视检查
	outputPath := filepath.Join(tmpDir, "test_output_single.png")
	err = imaging.Save(result, outputPath)
	if err != nil {
		t.Fatalf("保存结果图片失败: %v", err)
	}

	// 验证结果图片存在且大小正确
	stat, err := os.Stat(outputPath)
	assert.NoError(t, err)
	assert.True(t, stat.Size() > 0)
}

func TestDrawWatermarkGrid(t *testing.T) {
	// 准备测试图片
	tmpDir := t.TempDir()
	testImagePath := filepath.Join(tmpDir, "test_input.png")

	// 创建一个测试图片
	img := imaging.New(800, 600, color.White)
	err := imaging.Save(img, testImagePath)
	if err != nil {
		t.Fatalf("创建测试图片失败: %v", err)
	}

	// 加载测试图片
	baseImg, err := imaging.Open(testImagePath)
	if err != nil {
		t.Fatalf("加载测试图片失败: %v", err)
	}

	// 加载字体
	font := loadTestFont(t)

	// 创建水印配置
	config := TextConfig{
		Text:       "测试水印",
		Font:       font,
		FontSize:   36,
		Spacing:    10,
		RowSpacing: 10,
		ColSpacing: 20,
		Rows:       3,
		Cols:       4,
		DPI:        72,
		Color:      color.RGBA{R: 0, G: 0, B: 255, A: 255}, // 蓝色
		IsGrid:     true,
		Angle:      30,
	}

	// 测试网格水印
	result, err := DrawWatermarkGrid(baseImg, config, true)
	if err != nil {
		t.Fatalf("绘制网格水印失败: %v", err)
	}

	// 保存结果用于目视检查
	outputPath := filepath.Join(tmpDir, "test_output_grid.png")
	err = imaging.Save(result, outputPath)
	if err != nil {
		t.Fatalf("保存结果图片失败: %v", err)
	}

	// 验证结果图片存在且大小正确
	stat, err := os.Stat(outputPath)
	assert.NoError(t, err)
	assert.True(t, stat.Size() > 0)
}

func TestCalculateTextDimensions(t *testing.T) {
	// 加载字体
	font := loadTestFont(t)

	// 创建配置
	config := TextConfig{
		Text:     "测试文本",
		Font:     font,
		FontSize: 36,
		Spacing:  10,
		DPI:      72,
	}

	// 设置字体选项
	opts := truetype.Options{
		Size:    config.FontSize,
		DPI:     config.DPI,
		Hinting: xfont.HintingFull,
	}
	face := truetype.NewFace(font, &opts)
	defer face.Close()

	// 计算文字尺寸
	width, height := CalculateTextDimensions(config, face)

	// 验证结果
	assert.True(t, width > 0, "文字宽度应该大于0")
	assert.True(t, height > 0, "文字高度应该大于0")
}

func TestNormalizeAngle(t *testing.T) {
	tests := []struct {
		input    float64
		expected float64
	}{
		{0, 0},
		{180, 180},
		{-180, -180},
		{360, 0},
		{-360, 0},
		{540, 180},
		{-540, -180},
		{270, -90},
		{-270, 90},
	}

	for _, test := range tests {
		result := normalizeAngle(test.input)
		assert.Equal(t, test.expected, result, "输入角度 %f 应该归一化为 %f，但得到 %f",
			test.input, test.expected, result)
	}
}
