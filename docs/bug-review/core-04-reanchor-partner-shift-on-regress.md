# Re-anchor do input lùi đẩy track còn lại đi xa, timeline vượt wallclock vĩnh viễn
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** timeline, ingestor/pull (file), ingestor/tsnorm
- **Vị trí:** internal/timeline/normaliser.go:504-514,603-624; internal/ingestor/pull/file.go:178-183; internal/ingestor/tsnorm/tsnorm.go:650-664

## Hiện tượng
Khi nguồn lùi PTS trong cùng session, output PTS nhảy vượt wallclock đúng bằng khoảng lùi và giữ nguyên độ lệch đó. Với `file://x.ts` (mặc định loop), mỗi vòng cộng thêm một độ dài file, nên output ngày càng vượt xa wallclock. Nếu chỉ một track lùi, track còn lại bị lệch A/V vĩnh viễn.

Đây chính là tình trạng Normaliser được thiết kế để ngăn. Các consumer dựa vào wallclock như pacing RTSP và live edge DASH có thể bị ảnh hưởng.

## Điều kiện kích hoạt
Input DTS lùi hơn lượng nhỏ mà không có `OnSession`:
- TS file loop: `tsHandler` seek về đầu, không có PTS offset liên tục như MP4/FLV.
- HLS-pull hoặc UDP khi encoder restart hay phát lại mà kết nối vẫn sống.
- Trigger một track: PES không có PTS, `tsnorm.pesTimestampsMs` trả về `(0, 0)`.

## Phân tích
Ví dụ `now=1.000.040`, lùi `J=900.000`, input V=100.000.

1. V: `expected=100.000`, regress. `target=now`. `delta = target-expected = 900.040 > 0`, nên partner bị dịch:
```go
partner.outputAnchor += delta
partner.lastOutputDts += delta   // A.last ≈ 1.900.060
```
2. A: input cũng lùi, `expected≈now` (đúng), nhưng `expected < lastOutputDts` nên regress. `target <= last` nên `target = last+1 ≈ 1.900.061`. `delta≈900.016`, và V bị dịch ngược lại.
3. Từ đây cả hai ở `now+J`. `effActualNow = max(now, last)`, nên `drift` xấp xỉ một frame và không còn re-anchor. `MaxAheadMs` mặc định là 0 nên không có giới hạn trên.

Trường hợp chỉ V lùi (PES không PTS): A được dịch `+J`. Packet V kế tiếp nhảy tiến, bị re-anchor với `delta<0` nên không dịch lại A. Kết quả A lệch `+J` vĩnh viễn.

## Vì sao đây là lỗi
Comment nói "Only forward jumps are mirrored; a clamped re-anchor (delta <= 0)...". Nhưng khi `regressed`, `delta` luôn dương vì `target > lastOutputDts > expected`, nên điều kiện không phân biệt được hai trường hợp. `TestMonotonicFloor` chỉ có một track. `TestReanchorPreservesAVSync` chỉ kiểm tra stall (input tiếp tục tiến).

## Hướng sửa
Chỉ mirror khi output của track thật sự bị đẩy so với vị trí cũ (dùng `target - lastOutputDts` cũ của track, bỏ phần do input lùi), không dùng `target - expectedDts`. Khi `regressed` không nên dịch `partner.lastOutputDts`. Với PES không có PTS, nên bỏ qua hoặc suy ra timestamp thay vì trả về 0.

## Cách xác minh
Unit test hai track: chạy V/A đồng bộ 1000 giây theo wallclock, cho cả hai input lùi 900 giây, tiếp tục 10 giây, kiểm tra `output DTS - wallclock` < JumpThreshold. Thực tế: chạy `file://` TS ngắn có loop và xem PTS output sau vài vòng.
