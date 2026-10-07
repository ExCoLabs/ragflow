// Light stemmer for Slovak.
//
// Ported from the Slovak stemmer in wikimedia/search-extra
// (opensearch-extra-analysis-slovak), licensed by the Wikimedia Foundation
// under the Apache License, Version 2.0. That implementation follows the
// structure of the Apache Lucene "Light Stemmer for Czech"
// (org.apache.lucene.analysis.cz.CzechStemmer, Apache License 2.0), which
// implements Dolamic and Savoy, "Indexing and stemming approaches for the
// Czech language" (Information Processing & Management 45(6), 2009).
//
// The Slovak suffix inventory is adapted from stemm-sk, Copyright (c) 2015
// Marek Suppa, licensed under the MIT License:
//
//   Permission is hereby granted, free of charge, to any person obtaining a
//   copy of this software and associated documentation files (the
//   "Software"), to deal in the Software without restriction, including
//   without limitation the rights to use, copy, modify, merge, publish,
//   distribute, sublicense, and/or sell copies of the Software, and to
//   permit persons to whom the Software is furnished to do so, subject to
//   the following conditions:
//
//   The above copyright notice and this permission notice shall be included
//   in all copies or substantial portions of the Software.
//
// Snowball has no Slovak algorithm, so unlike every other language in
// Stemmer this one is implemented here rather than generated. The rules
// match accented letters, so the input must be lowercase and unfolded; it
// mirrors _SlovakStemmer in Infinity's rag_tokenizer.py token for token, and
// the copy in infinity's src/common/analyzer/stemmer/ line for line.

#pragma once

#include <cstddef>
#include <initializer_list>
#include <string>
#include <string_view>

namespace slovak_stemmer_detail {

inline std::u32string DecodeUtf8(const std::string &input) {
    std::u32string out;
    out.reserve(input.size());
    for (size_t i = 0; i < input.size();) {
        const unsigned char c = static_cast<unsigned char>(input[i]);
        size_t extra = 0;
        char32_t codepoint = 0;
        if (c < 0x80) {
            codepoint = c;
        } else if ((c & 0xE0) == 0xC0) {
            codepoint = c & 0x1F;
            extra = 1;
        } else if ((c & 0xF0) == 0xE0) {
            codepoint = c & 0x0F;
            extra = 2;
        } else if ((c & 0xF8) == 0xF0) {
            codepoint = c & 0x07;
            extra = 3;
        } else {
            // Not a UTF-8 lead byte; pass the byte through so the word is
            // returned unchanged rather than corrupted.
            out.push_back(c);
            i += 1;
            continue;
        }
        if (i + extra >= input.size()) {
            out.push_back(c);
            i += 1;
            continue;
        }
        for (size_t k = 1; k <= extra; ++k) {
            codepoint = (codepoint << 6) | (static_cast<unsigned char>(input[i + k]) & 0x3F);
        }
        out.push_back(codepoint);
        i += extra + 1;
    }
    return out;
}

inline std::string EncodeUtf8(const std::u32string &input, size_t length) {
    std::string out;
    out.reserve(length * 2);
    for (size_t i = 0; i < length; ++i) {
        const char32_t codepoint = input[i];
        if (codepoint < 0x80) {
            out += static_cast<char>(codepoint);
        } else if (codepoint < 0x800) {
            out += static_cast<char>(0xC0 | (codepoint >> 6));
            out += static_cast<char>(0x80 | (codepoint & 0x3F));
        } else if (codepoint < 0x10000) {
            out += static_cast<char>(0xE0 | (codepoint >> 12));
            out += static_cast<char>(0x80 | ((codepoint >> 6) & 0x3F));
            out += static_cast<char>(0x80 | (codepoint & 0x3F));
        } else {
            out += static_cast<char>(0xF0 | (codepoint >> 18));
            out += static_cast<char>(0x80 | ((codepoint >> 12) & 0x3F));
            out += static_cast<char>(0x80 | ((codepoint >> 6) & 0x3F));
            out += static_cast<char>(0x80 | (codepoint & 0x3F));
        }
    }
    return out;
}

inline bool EndsWith(const std::u32string &s, size_t len, const std::u32string_view suffix) {
    if (suffix.size() > len) {
        return false;
    }
    return s.compare(len - suffix.size(), suffix.size(), suffix) == 0;
}

inline bool EndsWithAny(const std::u32string &s, size_t len, std::initializer_list<std::u32string_view> suffixes) {
    for (const auto &suffix : suffixes) {
        if (EndsWith(s, len, suffix)) {
            return true;
        }
    }
    return false;
}

inline size_t Palatalise(std::u32string &s, size_t len) {
    if (EndsWithAny(s, len, {U"ci", U"ce", U"či", U"če"})) {
        s[len - 2] = U'k'; // [cč][ie] -> k
    } else if (EndsWithAny(s, len, {U"zi", U"ze", U"ži", U"že"})) {
        s[len - 2] = U'h'; // [zž][ie] -> h
    } else if (EndsWithAny(s, len, {U"čte", U"čti", U"čtí"})) {
        s[len - 3] = U'c'; // čt[eií] -> ck
        s[len - 2] = U'k';
    } else if (EndsWithAny(s, len, {U"šte", U"šti", U"ští"})) {
        s[len - 3] = U's'; // št[eií] -> sk
        s[len - 2] = U'k';
    }
    return len - 1;
}

inline size_t RemoveCase(std::u32string &s, size_t len) {
    if (len > 7 && EndsWith(s, len, U"atoch")) {
        return len - 5;
    }
    if (len > 6 && EndsWith(s, len, U"aťom")) {
        return Palatalise(s, len - 3);
    }
    if (len > 5) {
        if (EndsWithAny(s,
                        len,
                        {U"och", U"ich", U"ích", U"ého", U"ami", U"emi", U"ému", U"ete", U"eti", U"iho", U"ího", U"ími", U"imu", U"aťa"})) {
            return Palatalise(s, len - 2);
        }
        if (EndsWithAny(s, len, {U"ách", U"ata", U"aty", U"ých", U"ové", U"ovi", U"ými"})) {
            return len - 3;
        }
    }
    if (len > 4) {
        if (EndsWith(s, len, U"om")) {
            return Palatalise(s, len - 1);
        }
        if (EndsWithAny(s, len, {U"es", U"ém", U"ím"})) {
            return Palatalise(s, len - 2);
        }
        if (EndsWithAny(s, len, {U"úm", U"at", U"ám", U"os", U"us", U"ým", U"mi", U"ou", U"ej"})) {
            return len - 2;
        }
    }
    if (len > 3) {
        switch (s[len - 1]) {
            case U'e':
            case U'i':
            case U'í':
                return Palatalise(s, len);
            case U'ú':
            case U'y':
            case U'a':
            case U'o':
            case U'á':
            case U'é':
            case U'ý':
                return len - 1;
            default:
                break;
        }
    }
    return len;
}

inline size_t RemovePossessives(std::u32string &s, size_t len) {
    if (len > 5) {
        if (EndsWith(s, len, U"ov")) {
            return len - 2;
        }
        if (EndsWith(s, len, U"in")) {
            return Palatalise(s, len - 1);
        }
    }
    return len;
}

inline size_t RemovePrefixes(std::u32string &s, size_t len) {
    if (len > 5 && s.compare(0, 3, U"naj") == 0) {
        s.erase(0, 3);
        return len - 3;
    }
    return len;
}

} // namespace slovak_stemmer_detail

// Stem a lowercase Slovak word that still carries its diacritics.
inline std::string SlovakStem(const std::string &term) {
    namespace detail = slovak_stemmer_detail;
    std::u32string chars = detail::DecodeUtf8(term);
    size_t len = detail::RemoveCase(chars, chars.size());
    len = detail::RemovePossessives(chars, len);
    len = detail::RemovePrefixes(chars, len);
    return detail::EncodeUtf8(chars, len);
}
