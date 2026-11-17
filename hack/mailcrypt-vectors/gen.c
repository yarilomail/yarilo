/* Writes encrypted streams with the reference library for the refstream
   tests: one key per curve, every algorithm and flag set below, several
   sizes. MANIFEST lines: <stream> <key> <sha256 of plaintext>. */
#include "lib.h"
#include "buffer.h"
#include "hex-binary.h"
#include "ostream.h"
#include "randgen.h"
#include "sha2.h"
#include "dcrypt.h"
#include "dcrypt-iostream.h"
#include "ostream-encrypt.h"

#include <stdio.h>
#include <stdlib.h>

static const char *const curves[] = { "prime256v1", "secp384r1", "secp521r1", "X25519" };
static const struct {
	const char *name, *algo;
	enum io_stream_encrypt_flags flags;
	bool nist_only;
} modes[] = {
	{ "gcm", "aes-256-gcm-sha256", IO_STREAM_ENC_INTEGRITY_AEAD, FALSE },
	{ "gcm128", "aes-128-gcm-sha256", IO_STREAM_ENC_INTEGRITY_AEAD, FALSE },
	{ "gcmsame", "aes-256-gcm-sha256", IO_STREAM_ENC_INTEGRITY_AEAD | IO_STREAM_ENC_SAME_CALG, FALSE },
	{ "cbchmac", "aes-256-cbc-sha256", IO_STREAM_ENC_INTEGRITY_HMAC, FALSE },
	{ "cbcnone", "aes-256-cbc-sha256", IO_STREAM_ENC_INTEGRITY_NONE, FALSE },
	{ "v1", "aes-256-ctr-sha256", IO_STREAM_ENC_VERSION_1, TRUE },
};
static const size_t sizes[] = { 0, 1, 16, 70000 };

static void write_file(const char *path, const void *data, size_t size)
{
	FILE *f = fopen(path, "wb");
	if (f == NULL || fwrite(data, 1, size, f) != size || fclose(f) != 0)
		i_fatal("write %s", path);
}

int main(int argc, char **argv)
{
	const char *error;

	if (argc != 2)
		i_fatal("usage: gen <dir>");
	lib_init();
	if (!dcrypt_initialize("openssl", NULL, &error))
		i_fatal("dcrypt_initialize: %s", error);
	FILE *manifest = fopen(t_strdup_printf("%s/MANIFEST", argv[1]), "w");
	if (manifest == NULL)
		i_fatal("MANIFEST");

	for (unsigned int c = 0; c < N_ELEMENTS(curves); c++) T_BEGIN {
		struct dcrypt_keypair pair;
		if (!dcrypt_keypair_generate(&pair, DCRYPT_KEY_EC, 0, curves[c], &error))
			i_fatal("generate %s: %s", curves[c], error);
		buffer_t *pem = t_buffer_create(1024);
		if (!dcrypt_key_store_private(pair.priv, DCRYPT_FORMAT_PEM, NULL, pem, NULL, NULL, &error))
			i_fatal("store %s: %s", curves[c], error);
		const char *key = t_strdup_printf("%s.pem", curves[c]);
		write_file(t_strdup_printf("%s/%s", argv[1], key), pem->data, pem->used);

		for (unsigned int m = 0; m < N_ELEMENTS(modes); m++) {
			if (modes[m].nist_only && c == 3)
				continue;
			for (unsigned int s = 0; s < N_ELEMENTS(sizes); s++) {
				buffer_t *plain = t_buffer_create(sizes[s] + 1);
				random_fill(buffer_append_space_unsafe(plain, sizes[s]), sizes[s]);
				buffer_t *out = t_buffer_create(sizes[s] + 1024);
				struct ostream *os = o_stream_create_buffer(out);
				struct ostream *enc = o_stream_create_encrypt(os, modes[m].algo, pair.pub, modes[m].flags);
				o_stream_nsend(enc, plain->data, plain->used);
				if (o_stream_finish(enc) < 0)
					i_fatal("encrypt %s %s: %s", curves[c], modes[m].name, o_stream_get_error(enc));
				o_stream_destroy(&enc);
				o_stream_destroy(&os);

				const char *name = t_strdup_printf("%s-%s-%zu.bin", curves[c], modes[m].name, sizes[s]);
				write_file(t_strdup_printf("%s/%s", argv[1], name), out->data, out->used);
				unsigned char sum[SHA256_RESULTLEN];
				sha256_get_digest(plain->data, plain->used, sum);
				fprintf(manifest, "%s %s %s\n", name, key, binary_to_hex(sum, sizeof(sum)));
			}
		}
		dcrypt_keypair_unref(&pair);
	} T_END;
	fclose(manifest);
	dcrypt_deinitialize();
	lib_deinit();
	return 0;
}
