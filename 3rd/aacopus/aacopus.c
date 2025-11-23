#include <stdlib.h>
#include <string.h>

#include <libavcodec/avcodec.h>
#include <libavutil/avutil.h>
#include <libavutil/mem.h>
#include <libavutil/channel_layout.h>
#include <libswresample/swresample.h>

#include "aacopus.h"

typedef struct EncodedNode {
    struct EncodedNode* next;
    int size;
    uint8_t* data;
} EncodedNode;

struct AACOpusTranscoder {
    AVCodecContext* dec;
    AVCodecContext* enc;
    SwrContext* swr;
    AVFrame* frame;
    AVPacket* pkt;
    AVPacket* enc_pkt;
    EncodedNode* head;
    EncodedNode* tail;
};

static void free_queue(AACOpusTranscoder* t) {
    EncodedNode* n = t->head;
    while (n) {
        EncodedNode* next = n->next;
        av_free(n->data);
        av_free(n);
        n = next;
    }
    t->head = t->tail = NULL;
}

void aacopus_free(AACOpusTranscoder* t) {
    if (!t) return;
    free_queue(t);
    if (t->frame) av_frame_free(&t->frame);
    if (t->pkt) av_packet_free(&t->pkt);
    if (t->enc_pkt) av_packet_free(&t->enc_pkt);
    if (t->dec) avcodec_free_context(&t->dec);
    if (t->enc) avcodec_free_context(&t->enc);
    if (t->swr) swr_free(&t->swr);
    av_free(t);
}

static int queue_packet(AACOpusTranscoder* t, const AVPacket* pkt) {
    EncodedNode* n = av_mallocz(sizeof(*n));
    if (!n) return AVERROR(ENOMEM);
    n->data = av_malloc(pkt->size);
    if (!n->data) {
        av_free(n);
        return AVERROR(ENOMEM);
    }
    memcpy(n->data, pkt->data, pkt->size);
    n->size = pkt->size;
    n->next = NULL;
    if (!t->tail) {
        t->head = t->tail = n;
    } else {
        t->tail->next = n;
        t->tail = n;
    }
    return 0;
}

AACOpusTranscoder* aacopus_new(int opus_sample_rate, int opus_channels, int opus_bitrate,
                               const uint8_t* asc, int asc_size) {
    AACOpusTranscoder* t = av_mallocz(sizeof(*t));
    const AVCodec* dec;
    const AVCodec* enc;
    if (!t) return NULL;
    avcodec_register_all();
    dec = avcodec_find_decoder(AV_CODEC_ID_AAC);
    enc = avcodec_find_encoder(AV_CODEC_ID_OPUS);
    if (!dec || !enc) {
        aacopus_free(t);
        return NULL;
    }
    t->dec = avcodec_alloc_context3(dec);
    t->enc = avcodec_alloc_context3(enc);
    if (!t->dec || !t->enc) {
        aacopus_free(t);
        return NULL;
    }
    if (asc && asc_size > 0) {
        t->dec->extradata = av_mallocz(asc_size + AV_INPUT_BUFFER_PADDING_SIZE);
        if (!t->dec->extradata) {
            aacopus_free(t);
            return NULL;
        }
        memcpy(t->dec->extradata, asc, asc_size);
        t->dec->extradata_size = asc_size;
    }
    if (avcodec_open2(t->dec, dec, NULL) < 0) {
        aacopus_free(t);
        return NULL;
    }
    t->enc->sample_rate = opus_sample_rate > 0 ? opus_sample_rate : 48000;
    t->enc->channels = opus_channels > 0 ? opus_channels : 2;
    t->enc->channel_layout = av_get_default_channel_layout(t->enc->channels);
    t->enc->bit_rate = opus_bitrate > 0 ? opus_bitrate : 64000;
    t->enc->time_base = (AVRational){1, t->enc->sample_rate};
    t->enc->sample_fmt = enc->sample_fmts ? enc->sample_fmts[0] : AV_SAMPLE_FMT_FLTP;
    t->enc->strict_std_compliance = FF_COMPLIANCE_EXPERIMENTAL;
    if (avcodec_open2(t->enc, enc, NULL) < 0) {
        aacopus_free(t);
        return NULL;
    }
    t->swr = swr_alloc_set_opts(NULL,
                                t->enc->channel_layout, t->enc->sample_fmt, t->enc->sample_rate,
                                t->dec->channel_layout ? t->dec->channel_layout : av_get_default_channel_layout(t->dec->channels),
                                t->dec->sample_fmt, t->dec->sample_rate,
                                0, NULL);
    if (!t->swr || swr_init(t->swr) < 0) {
        aacopus_free(t);
        return NULL;
    }
    t->frame = av_frame_alloc();
    t->pkt = av_packet_alloc();
    t->enc_pkt = av_packet_alloc();
    if (!t->frame || !t->pkt || !t->enc_pkt) {
        aacopus_free(t);
        return NULL;
    }
    return t;
}

int aacopus_feed(AACOpusTranscoder* t, const uint8_t* data, int size) {
    int ret;
    if (!t || !data || size <= 0) return 0;
    av_packet_unref(t->pkt);
    t->pkt->data = (uint8_t*)data;
    t->pkt->size = size;
    ret = avcodec_send_packet(t->dec, t->pkt);
    if (ret < 0 && ret != AVERROR(EAGAIN) && ret != AVERROR_EOF) return ret;
    for (;;) {
        av_frame_unref(t->frame);
        ret = avcodec_receive_frame(t->dec, t->frame);
        if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) break;
        if (ret < 0) return ret;
        int64_t delay = swr_get_delay(t->swr, t->dec->sample_rate);
        int dst_nb_samples = av_rescale_rnd(delay + t->frame->nb_samples,
                                            t->enc->sample_rate,
                                            t->dec->sample_rate,
                                            AV_ROUND_UP);
        AVFrame* out = av_frame_alloc();
        if (!out) return AVERROR(ENOMEM);
        out->channel_layout = t->enc->channel_layout;
        out->format = t->enc->sample_fmt;
        out->sample_rate = t->enc->sample_rate;
        out->nb_samples = dst_nb_samples;
        if ((ret = av_frame_get_buffer(out, 0)) < 0) {
            av_frame_free(&out);
            return ret;
        }
        ret = swr_convert(t->swr, out->data, out->nb_samples,
                          (const uint8_t**)t->frame->data, t->frame->nb_samples);
        if (ret < 0) {
            av_frame_free(&out);
            return ret;
        }
        out->nb_samples = ret;
        ret = avcodec_send_frame(t->enc, out);
        av_frame_free(&out);
        if (ret < 0 && ret != AVERROR(EAGAIN) && ret != AVERROR_EOF) return ret;
        for (;;) {
            av_packet_unref(t->enc_pkt);
            ret = avcodec_receive_packet(t->enc, t->enc_pkt);
            if (ret == AVERROR(EAGAIN) || ret == AVERROR_EOF) break;
            if (ret < 0) return ret;
            ret = queue_packet(t, t->enc_pkt);
            if (ret < 0) return ret;
        }
    }
    return 0;
}

int aacopus_get(AACOpusTranscoder* t, uint8_t* out, int out_size) {
    EncodedNode* n;
    if (!t || !out || out_size <= 0) return 0;
    n = t->head;
    if (!n) return 0;
    if (n->size > out_size) return AVERROR(EINVAL);
    memcpy(out, n->data, n->size);
    int size = n->size;
    t->head = n->next;
    if (!t->head) t->tail = NULL;
    av_free(n->data);
    av_free(n);
    return size;
}

