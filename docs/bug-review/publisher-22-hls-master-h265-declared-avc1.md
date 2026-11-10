# HLS master khai `avc1` cho rendition H.265
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (HLS ABR), transcoder
- **Vị trí:** internal/publisher/hls.go:483-486, 475-477, 845-857; internal/publisher/hls_abr.go:169-172; internal/publisher/ts_keyframe_scanner.go:131-135

## Hiện tượng
Ladder H.265 không phát được hoặc phát lỗi: hls.js tạo SourceBuffer với codec `avc1` rồi gặp HEVC, Safari có thể loại variant vì CODECS sai.

## Điều kiện kích hoạt
Transcoder cấu hình `codec: h265` (ví dụ `hevc_nvenc`), hoặc rendition passthrough HEVC được phát qua ABR.

## Phân tích
Codec thật chỉ được đọc với H.264:
```go
if !av.KeyFrame || av.Codec != domain.AVCodecH264 { return }
```
Scanner raw-TS cũng chỉ cập nhật `h264Codec`. Với HEVC, `videoCodec` vẫn rỗng và `writeHLSMasterPlaylist` rơi về `hlsCodecString`, hàm chỉ có các chuỗi `avc1.*` theo độ phân giải.

## Vì sao đây là lỗi
ARCHITECTURE và `ResolveVideoEncoder` xác nhận H.265 là cấu hình hợp lệ, HLS TS mang được HEVC (`FromAV` dùng `StreamTypeH265Video`). Master khai sai ngay từ đầu; fallback không biết codec thật.

## Hướng sửa
Parse SPS HEVC để tạo `hvc1.*`/`hev1.*` (DASH đã có `hevc.CodecString`). Với fallback, chọn theo codec cấu hình và bỏ CODECS nếu chưa biết.

## Cách xác minh
Unit test với AV packet H.265 có SPS: master phải có `hvc1`. Thực tế: tạo profile `h265`, kiểm tra `index.m3u8`.
