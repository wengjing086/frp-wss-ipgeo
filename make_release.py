import os, json, glob, urllib.request, urllib.error, urllib.parse

TOKEN = os.environ['GITHUB_TOKEN']
REPO = "wengjing086/frp-wss-ipgeo"
API = "https://api.github.com"

with open('C:/Users/xnj/frp-wss-ipgeo-pub/release_body.md', encoding='utf-8') as f:
    body = f.read()

data = json.dumps({
    "tag_name": "v0.1.0",
    "name": "v0.1.0 - frp WSS + China IP geolocation",
    "body": body,
    "draft": False,
    "prerelease": False,
}).encode('utf-8')

req = urllib.request.Request(f"{API}/repos/{REPO}/releases", data=data, method='POST')
req.add_header("Authorization", f"Bearer {TOKEN}")
req.add_header("Accept", "application/vnd.github+json")
req.add_header("X-GitHub-Api-Version", "2022-11-28")
req.add_header("Content-Type", "application/json")
with urllib.request.urlopen(req) as r:
    rel = json.load(r)
release_id = rel['id']
print("release id", release_id, "html_url", rel.get("html_url"))
upload_url = rel['upload_url'].split('{')[0]
print("upload_url", upload_url)

bin_dir = 'C:/Users/xnj/frp-wss-ipgeo-pub/bin'
for path in sorted(glob.glob(bin_dir + '/*')):
    name = os.path.basename(path)
    with open(path, 'rb') as fp:
        content = fp.read()
    url = f"{upload_url}?name={urllib.parse.quote(name)}"
    req2 = urllib.request.Request(url, data=content, method='POST')
    req2.add_header("Authorization", f"Bearer {TOKEN}")
    req2.add_header("Content-Type", "application/octet-stream")
    try:
        with urllib.request.urlopen(req2) as r2:
            info = json.load(r2)
            print("uploaded OK:", info.get('name'), info.get('size'))
    except urllib.error.HTTPError as e:
        print("FAILED", name, e.code, e.read().decode('utf-8', 'replace')[:300])
