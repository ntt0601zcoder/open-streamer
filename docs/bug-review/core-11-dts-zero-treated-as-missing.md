# DTS=0 hợp lệ bị coi là thiếu, DTS trên wire lùi ở frame thứ hai khi có B-frame
- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** tsmux, ingestor/tsnorm, timeline
- **Vị trí:** internal/tsmux/fromav.go:144-146,206-209; internal/ingestor/tsnorm/tsnorm.go:499-506; internal/timeline/normaliser.go:694-708,929-939

## Hiện tượng
Packet video đầu của session có DTS lớn hơn DTS của frame kế tiếp trên wire. Muxer hoặc analyzer chặt chẽ báo DTS không đơn điệu ở đầu stream.

## Điều kiện kích hoạt
Video có B-frame và là track được joint seed anchor tại 0, nghĩa là DTS output của IDR đầu bằng 0 và `cto>0`.

## Phân tích
Normaliser xuất DTS=0, PTS=80. Muxer coi 0 là thiếu:
```go
dts := av.DTSms
if dts == 0 { dts = av.PTSms }
```
IDR ra wire với DTS ngầm định 80 và PCR 80. Frame kế tiếp có DTS=40, tức lùi 40 ms. `FromAV` cũng tương tự (`p.DTSms != 0 &&`).

## Vì sao đây là lỗi
Joint seed cố ý anchor tại 0 nên DTS=0 là giá trị hợp lệ. Muxer không phân biệt được 0 hợp lệ với 0 do thiếu. Việc thiếu DTS đã được xử lý ở biên demuxer (`dtsMs = ptsMs`).

## Hướng sửa
Bỏ thay thế `DTS==0` trong muxer: chỉ dùng `DTS != PTS` để chọn indicator. Nếu thay đổi joint seed theo core-03 thì tình huống này gần như chỉ còn ở session đầu.

## Cách xác minh
Mux IDR (DTS=0, PTS=80) rồi P (DTS=40, PTS=240), demux lại và kiểm tra DTS không giảm.
