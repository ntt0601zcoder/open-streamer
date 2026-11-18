// Package flv turns hub frames into RTMP/FLV messages: the decoder
// configuration records of the sequence headers, and the per-connection
// Muxer that rebases timestamps and sends onMetaData and headers per epoch.
package flv
