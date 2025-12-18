#ifndef AACOPUS_H
#define AACOPUS_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct AACOpusTranscoder AACOpusTranscoder;

/**
 * Create AAC->Opus transcoder.
 *
 * opus_sample_rate: output sample rate (e.g. 48000)
 * opus_channels: number of output channels (1 or 2)
 * opus_bitrate: target bitrate in bits per second (e.g. 64000)
 * asc: MPEG-4 AudioSpecificConfig bytes of the AAC stream
 * asc_size: length of asc
 */
AACOpusTranscoder* aacopus_new(int opus_sample_rate, int opus_channels, int opus_bitrate,
                               const uint8_t* asc, int asc_size);

/**
 * Feed one AAC access unit.
 *
 * Returns 0 on success, negative AVERROR code on failure.
 */
int aacopus_feed(AACOpusTranscoder* t, const uint8_t* data, int size);

/**
 * Get one encoded Opus packet.
 *
 * Copies at most out_size bytes to out and returns number of bytes copied,
 * 0 if no packet is available, or negative AVERROR code on failure.
 */
int aacopus_get(AACOpusTranscoder* t, uint8_t* out, int out_size);

/**
 * Destroy transcoder and free resources.
 */
void aacopus_free(AACOpusTranscoder* t);

#ifdef __cplusplus
}
#endif

#endif /* AACOPUS_H */

