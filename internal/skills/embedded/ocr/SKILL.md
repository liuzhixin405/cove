# OCR Image Reader Skill

Extract text from image files using OCR. Supports `.jpg`, `.jpeg`, `.png`, `.gif`, `.bmp`, `.webp`, `.tiff`.

## When to Use
- User attaches or references an image file path (e.g. `.jpg`, `.png`)
- User asks "what does this image say" or "read this screenshot"
- Any time you need to extract text from an image

## Workflow

### Step 0: Analyze image properties FIRST
Before running OCR, always analyze the image to choose the best strategy:
```python
from PIL import Image
img = Image.open('path')
print(f'Size: {img.size}, Mode: {img.mode}')

# Sample colors to detect dark/light theme
from collections import Counter
pixels = list(img.getdata())[::100]  # sample every 100th pixel
colors = Counter((p[0]//30*30, p[1]//30*30, p[2]//30*30) for p in pixels)
for c, n in colors.most_common(5):
    b = sum(c)/3
    print(f'  RGB{c}: {n/len(pixels)*100:.0f}% ({"dark" if b<128 else "light"})')
```

### Step 1: Choose OCR strategy based on image analysis

#### Strategy A: Terminal screenshots (dark blue/black bg + white text)
- **Use RED channel** for isolation (dark bg has R≈0, white text has R≈255)
- Threshold and upscale 4x:
```python
r, g, b = img.resize((w*4, h*4), Image.LANCZOS).split()
mask = r.point(lambda x: 255 if x > 40 else 0)
```

#### Strategy B: Light background screenshots (white/light bg + dark text)
- Direct grayscale + threshold:
```python
gray = img.convert('L')
mask = gray.point(lambda x: 0 if x > 128 else 255)
```

#### Strategy C: General purpose
- Try easyocr first (best quality, but slow first run - downloads ~100MB models):
```python
import easyocr
reader = easyocr.Reader(['en'], gpu=False)
text = reader.readtext(img, detail=0)
```
- Fall back to tesseract with preprocessing

### Step 2: Tesseract with preprocessing
```bash
# Install once:
winget install UB-Mannheim.TesseractOCR          # Windows
brew install tesseract                             # macOS
sudo apt install tesseract-ocr                     # Linux
```

```powershell
python -m pip install pytesseract pillow -q
```

```python
import pytesseract
from PIL import Image

pytesseract.pytesseract.tesseract_cmd = r'C:\Program Files\Tesseract-OCR\tesseract.exe'

# Preprocess (choose based on Step 0 analysis)
img = Image.open('path')
img = img.resize((img.width*4, img.height*4), Image.LANCZOS)  # upscale
# Apply appropriate mask (see strategies above)

# Try PSM modes
for psm in [3, 6, 11]:
    text = pytesseract.image_to_string(img, lang='eng', config=f'--psm {psm}')
    if text.strip():
        print(text)
```

### Step 3: Known limitations
- **Small text** (<20px char height): OCR unreliable. Ask user for larger screenshot or describe content.
- **Terminal screenshots**: Must preprocess (red channel isolation + threshold) before tesseract
- **easyocr first run**: Downloads ~100MB model, takes 1-2 minutes
- **Chinese text**: Need `chi_sim.traineddata` for tesseract or `['ch_sim']` for easyocr

### Step 4: If ALL OCR fails
- Tell user OCR failed and explain why (text too small, unusual font, etc.)
- Ask user to describe the image content
- Suggest user take a larger/higher-res screenshot
