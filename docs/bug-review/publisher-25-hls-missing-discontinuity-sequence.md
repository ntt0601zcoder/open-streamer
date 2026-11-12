# Playlist HLS live thiếu `EXT-X-DISCONTINUITY-SEQUENCE`
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (HLS)
- **Vị trí:** internal/publisher/hls.go:796-807

## Hiện tượng
Khi tag DISCONTINUITY trượt khỏi cửa sổ, player tính sai số thứ tự discontinuity. Với ABR, các variant có thể có đếm discontinuity khác nhau và căn chỉnh sai khi đổi chất lượng.

## Điều kiện kích hoạt
Có failover hoặc force-flush tạo DISCONTINUITY, và segment đó trượt khỏi cửa sổ playlist.

## Phân tích
`writeManifestLocked` chỉ ghi `EXTM3U`, `VERSION`, `TARGETDURATION`, `MEDIA-SEQUENCE`, không có `EXT-X-DISCONTINUITY-SEQUENCE`. `hlsSegEntry.disc` bị loại khỏi `win` mà không có bộ đếm tích lũy.

## Vì sao đây là lỗi
RFC 8216 mục 4.3.3.3 yêu cầu server tăng `EXT-X-DISCONTINUITY-SEQUENCE` khi bỏ tag DISCONTINUITY khỏi playlist. Không có giá trị này, mọi segment được coi thuộc sequence bắt đầu từ 0 sau khi tag trượt ra.

## Hướng sửa
Giữ bộ đếm số tag đã trượt khỏi `win` và ghi tag này trước segment đầu tiên.

## Cách xác minh
Unit test: tạo discontinuity rồi đẩy nó ra khỏi cửa sổ, playlist phải có `#EXT-X-DISCONTINUITY-SEQUENCE:1`.
