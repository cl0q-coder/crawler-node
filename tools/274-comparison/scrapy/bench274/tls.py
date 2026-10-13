"""Test-only TLS context factory: skip certificate verification against the
throwaway fixture CA (mirrors the baseline harness's InsecureSkipVerify).
The TLS handshake crypto is still fully exercised."""
from scrapy.core.downloader.contextfactory import ScrapyClientContextFactory


class InsecureContextFactory(ScrapyClientContextFactory):
    def getContext(self, hostname=None, port=None):
        ctx = super().getContext(hostname, port)
        try:
            from OpenSSL import SSL

            ctx.set_verify(SSL.VERIFY_NONE, lambda *args: True)
        except Exception:
            pass
        return ctx
