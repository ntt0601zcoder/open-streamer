# RTSP gọi DTSExtractor trước khi dùng SPS từ SDP, nguồn chỉ khai báo tham số trong SDP mất toàn bộ hình
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/pull, thư viện mediacommon
- **Vị trí:** internal/ingestor/pull/rtsp.go:214-219, 236-240, 260-266, 315-321, 338-342, 361-363; mediacommon v2.8.3 pkg/codecs/h264/dts_extractor.go:150-152; pkg/codecs/h265/dts_extractor.go:212-217

## Hiện tượng
Camera hoặc server RTSP chỉ gửi SPS/PPS trong SDP (`sprop-parameter-sets`) và không lặp lại trong luồng RTP: kết nối thành công, audio (nếu có) vẫn chạy, nhưng không có video nào vào buffer. Không có log mức Warn.

## Điều kiện kích hoạt
Input `rtsp://` với H.264 có SPS trong SDP và các IDR chỉ chứa slice. Với H.265 tương tự, khi VPS/SPS/PPS chỉ có trong SDP.

## Phân tích
Cache được nạp từ SDP đúng như thiết kế:
```go
cachedSPS := append([]byte(nil), forma.SPS...)
```
Nhưng `dtsEx` không được cấp SPS đó, và được gọi trên AU gốc trước bước chèn prefix:
```go
isIDR := mch264.IsRandomAccess(au)
dts, err := dtsEx.Extract(au, pts)
if err != nil {
    return
}
```
`DTSExtractor` của mediacommon chỉ học SPS từ NAL trong AU truyền vào:
```go
if d.spsp == nil {
    return 0, false, fmt.Errorf("SPS not received yet")
}
```
Vì nguồn không bao giờ gửi SPS inline, mọi AU đều lỗi và callback `return` im lặng. Nhánh prepend `case len(cachedSPS) > 0 && len(cachedPPS) > 0` không bao giờ chạy tới.

H.265 có cùng thứ tự: `Extract` ở dòng 339, prepend ở dòng 363.

Cùng đoạn code còn nuốt mọi lỗi `Extract` khác, ví dụ `pic_order_cnt_type = 1 is not supported yet`, nên nguồn dùng POC type 1 cũng mất hình mà không có log.

## Vì sao đây là lỗi
`ARCHITECTURE.md` ghi invariant "RTSP: H264/H265 callbacks ... cache from SDP + inline-scan + drop fallback", nhưng đường SDP thực tế không đạt được. Ví dụ chính thức của gortsplib thêm SPS/PPS vào AU trước khi gọi extractor, nên đây không phải giới hạn của thư viện. Bộ đếm `droppedCount` nằm sau `Extract` nên không phát hiện được.

## Hướng sửa
Với IDR, ghép `cachedSPS/cachedPPS` (H.265 thêm VPS) vào AU trước khi `Extract`. Log có giới hạn tần suất khi `Extract` lỗi. Có thể fallback `dts = pts` khi không xác định được.

## Cách xác minh
Unit test tách callback: `Initialize` `DTSExtractor`, gọi `Extract` với AU chỉ có IDR: nhận lỗi. Thủ công trên môi trường thử: chạy RTSP server với `ffmpeg -bsf:v h264_metadata`/filter loại SPS/PPS inline nhưng giữ SDP, kiểm tra tracks chỉ có audio.
