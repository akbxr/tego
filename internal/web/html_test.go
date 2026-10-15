package web

import (
	"net/url"
	"strings"
	"testing"
)

const articlePage = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Fallback Title &amp; Co</title>
  <meta property="og:title" content="OG Title">
  <style>body { color: red; }</style>
  <script>var tracking = "<p>not text</p>";</script>
</head>
<body>
  <header class="site"><a href="/">Home</a> <a href="/blog">Blog</a></header>
  <nav><ul><li><a href="/docs">Docs nav</a></li></ul></nav>
  <div class="layout">
    <aside>Related posts</aside>
    <article class="post">
      <header><h1>Parsing   HTML <em>by hand</em></h1><p class="byline">By Ana &mdash; 3&nbsp;min</p></header>
      <p>Read the <a href="../guide/intro.html?a=1&amp;b=2">intro guide</a> first, or the
         <a href="https://example.org/abs">absolute link</a>.<br>Next line &#8212; &#x2603; &copy; &lt;tag&gt;</p>
      <p><a href="#section"><svg viewBox="0 0 1 1"><path d="M0"/></svg></a>Anchor-only links stay plain: <a href="#top">top</a>.</p>
      <h2>Steps</h2>
      <ol>
        <li>Tokenize</li>
        <li>Walk the <code>tokens</code>
          <ul><li>skip scripts</li><li>keep text</li></ul>
        </li>
      </ol>
      <pre><code class="language-go">func main() {
	fmt.Println("a &lt; b")
}
</code></pre>
      <table>
        <thead><tr><th>Name</th><th>Value</th></tr></thead>
        <tbody><tr><td>alpha</td><td><b>1</b></td></tr><tr><td>beta</td><td>2</td></tr></tbody>
      </table>
      <div hidden>secret hidden text</div>
      <noscript>Enable JavaScript</noscript>
      <form><input name="q"><button>Search</button></form>
    </article>
  </div>
  <footer>Copyright footer</footer>
</body>
</html>`

func TestExtractArticle(t *testing.T) {
	base, _ := url.Parse("https://blog.example.com/posts/2026/parsing.html")
	doc := extract(articlePage, base)
	if doc.Title != "Fallback Title & Co" {
		t.Errorf("title = %q", doc.Title)
	}
	want := "# Parsing HTML by hand\n\n" +
		"By Ana — 3 min\n\n" +
		"Read the [intro guide](https://blog.example.com/posts/guide/intro.html?a=1&b=2) first, or the [absolute link](https://example.org/abs).\n" +
		"Next line — ☃ © <tag>\n\n" +
		"Anchor-only links stay plain: top.\n\n" +
		"## Steps\n\n" +
		"1. Tokenize\n" +
		"2. Walk the `tokens`\n" +
		"  - skip scripts\n" +
		"  - keep text\n\n" +
		"```go\nfunc main() {\n\tfmt.Println(\"a < b\")\n}\n```\n\n" +
		"| Name | Value |\n" +
		"| --- | --- |\n" +
		"| alpha | 1 |\n" +
		"| beta | 2 |"
	if doc.Text != want {
		t.Errorf("text mismatch\n--- got ---\n%s\n--- want ---\n%s", doc.Text, want)
	}
	for _, bad := range []string{"tracking", "Docs nav", "Related posts", "Copyright", "secret", "Enable JavaScript", "Search", "color: red", "Home"} {
		if strings.Contains(doc.Text, bad) {
			t.Errorf("text contains dropped content %q", bad)
		}
	}
}

func TestExtractWithoutArticle(t *testing.T) {
	page := `<html><head><meta property="og:title" content="OG &quot;T&quot;"><base href="https://x.dev/sub/"></head><body>
<header>Site header</header>
<main><p>Main <a href="page">link</a> text that is long enough to be the page body. ` + strings.Repeat("More words here. ", 20) + `</p></main>
<footer>Footer</footer></body></html>`
	doc := extract(page, nil)
	if doc.Title != `OG "T"` {
		t.Errorf("title = %q", doc.Title)
	}
	if !strings.HasPrefix(doc.Text, "Main [link](https://x.dev/sub/page) text") {
		t.Errorf("text = %q", doc.Text)
	}
	if strings.Contains(doc.Text, "Site header") || strings.Contains(doc.Text, "Footer") {
		t.Errorf("header/footer kept: %q", doc.Text)
	}
}

func TestDecodeEntities(t *testing.T) {
	for in, want := range map[string]string{
		"a &amp b":          "a & b",
		"&#128512;&#x41;":   "😀A",
		"&#150;":            "–",
		"&unknown; &":       "&unknown; &",
		"x &lt;y&gt; &quot": "x <y> \"",
	} {
		if got := decodeEntities(in); got != want {
			t.Errorf("decodeEntities(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeCharsetLatin1(t *testing.T) {
	if got := decodeCharset([]byte("caf\xe9 \x93q\x94"), "ISO-8859-1"); got != "café “q”" {
		t.Errorf("got %q", got)
	}
}
