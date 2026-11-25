#include <cstdio>
#include <cstring>
#include <iostream>
#include <libetonyek/libetonyek.h>
#include <librevenge-generators/librevenge-generators.h>
#include <librevenge-stream/librevenge-stream.h>
#include <sys/resource.h>

// The library owns Apple's XML/IWA schemas. This adapter only preserves section
// boundaries in a bounded JSON contract; it performs no inference or networking.
static void quoted(const char *text) {
  std::cout << '"';
  for (const unsigned char *p = reinterpret_cast<const unsigned char *>(text); *p; ++p) {
    switch (*p) {
      case '"': std::cout << "\\\""; break;
      case '\\': std::cout << "\\\\"; break;
      case '\n': std::cout << "\\n"; break;
      case '\r': std::cout << "\\r"; break;
      case '\t': std::cout << "\\t"; break;
      default:
        if (*p < 32) { char escaped[7]; std::snprintf(escaped, sizeof escaped, "\\u%04x", *p); std::cout << escaped; }
        else std::cout << *p;
    }
  }
  std::cout << '"';
}
static void section(unsigned number, const char *text) {
  std::cout << "{\"number\":" << number << ",\"blocks\":[{\"kind\":\"text\",\"text\":";
  quoted(text); std::cout << "}]}";
}
int main(int argc, char **argv) try {
  if (argc != 2) return 2;
  struct rlimit memory = {512UL << 20, 512UL << 20};
  if (setrlimit(RLIMIT_AS, &memory)) return 2;
  librevenge::RVNGFileStream input(argv[1]);
  using libetonyek::EtonyekDocument;
  EtonyekDocument::Type type = EtonyekDocument::TYPE_UNKNOWN;
  if (EtonyekDocument::isSupported(&input, &type) == EtonyekDocument::CONFIDENCE_NONE) return 1;
  if (type == EtonyekDocument::TYPE_KEYNOTE) {
    librevenge::RVNGStringVector slides;
    librevenge::RVNGTextPresentationGenerator generator(slides);
    if (!EtonyekDocument::parse(&input, &generator) || slides.empty() || slides.size() > 500) return 1;
    std::cout << "{\"extractor\":\"libetonyek 0.1.10 iWork text v1\",\"unit\":\"slide\",\"sections\":[";
    for (unsigned i = 0; i < slides.size(); ++i) { if (i) std::cout << ','; section(i+1, slides[i].cstr()); }
  } else if (type == EtonyekDocument::TYPE_PAGES) {
    librevenge::RVNGString text;
    librevenge::RVNGTextTextGenerator generator(text);
    if (!EtonyekDocument::parse(&input, &generator)) return 1;
    std::cout << "{\"extractor\":\"libetonyek 0.1.10 iWork text v1\",\"unit\":\"section\",\"sections\":[";
    section(1, text.cstr());
  } else return 1;
  std::cout << "]}";
  return std::cout.good() ? 0 : 1;
} catch (...) { return 1; }
