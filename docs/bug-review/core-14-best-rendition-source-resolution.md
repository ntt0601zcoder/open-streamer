# BestRenditionIndex coi profile giữ độ phân giải nguồn (0x0) là thấp nhất
- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** buffer, domain
- **Vị trí:** internal/buffer/rendition.go:69-83; internal/domain/transcoder.go:74

## Hiện tượng
Ladder có một rung giữ nguyên độ phân giải nguồn và các rung thấp hơn: RTMP, RTSP, SRT, push và DVR phát rung thấp hơn thay vì rung tốt nhất.

## Điều kiện kích hoạt
Ví dụ profiles `[{1280x720, 2500}, {0x0, 8000}]`.

## Phân tích
```go
sc := rends[i].Width*rends[i].Height*1000 + rends[i].BitrateKbps
```
Điểm của 720p là 921.602.500, của rung nguồn là 8.000. Rung 720p được chọn. Một profile chỉ đặt một chiều cũng có điểm bằng bitrate.

## Vì sao đây là lỗi
`VideoProfile` ghi "Set to 0 to keep the source dimensions", nên 0 nghĩa là theo nguồn chứ không phải nhỏ nhất.

## Hướng sửa
So sánh rung 0x0 bằng bitrate, hoặc coi 0x0 là cao nhất; với profile chỉ có một chiều thì so sánh theo chiều đó.

## Cách xác minh
Unit test `BestRenditionIndex` với hai rung ở trên, kỳ vọng index 1.
