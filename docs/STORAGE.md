# Storage

Files at `/storage/v1`, compatible with `supabase.storage`. Files live on the server's disk, in the data directory, or in an S3-compatible bucket (AWS S3, Cloudflare R2, Backblaze B2, MinIO, Hetzner). Pick the backend under **Storage → Settings**; it applies to every project on the server.

```ts
await supabase.storage.createBucket("avatars", { public: true, fileSizeLimit: "5MB", allowedMimeTypes: ["image/*"] });
await supabase.storage.from("avatars").upload(`${user.id}/me.png`, file, { upsert: true });
const { data } = supabase.storage.from("avatars").getPublicUrl(`${user.id}/me.png`);
const { data: signed } = await supabase.storage.from("private").createSignedUrl("report.pdf", 3600);
```

## Buckets and policies

- **Public** buckets serve files to anyone with the URL. **Private** buckets need a JWT or a signed URL.
- Per-bucket **file size limit** and **allowed mime types** (`image/*` works).
- Access is decided by **RLS on `storage.objects`**, exactly like Supabase. For example, users manage only their own folder:

```sql
create policy "own folder" on storage.objects for all to authenticated
  using (bucket_id = 'avatars' and (storage.foldername(name))[1] = auth.uid()::text)
  with check (bucket_id = 'avatars' and (storage.foldername(name))[1] = auth.uid()::text);
```

The service_role key bypasses policies. The **Storage** page in the UI is a file browser with drag-and-drop upload.

## Resumable uploads

For large files and flaky mobile networks, use the TUS endpoint `/storage/v1/upload/resumable` with [tus-js-client](https://github.com/tus/tus-js-client) or Uppy, as in Supabase's docs:

```ts
import * as tus from "tus-js-client";

const upload = new tus.Upload(file, {
  endpoint: "https://shop.base.example.com/storage/v1/upload/resumable",
  headers: { authorization: `Bearer ${session.access_token}`, "x-upsert": "true" },
  chunkSize: 6 * 1024 * 1024,
  metadata: { bucketName: "videos", objectName: `${user.id}/clip.mp4`, contentType: "video/mp4" },
  onProgress: (sent, total) => console.log(Math.round((sent / total) * 100) + "%"),
});
upload.findPreviousUploads().then((prev) => { if (prev.length) upload.resumeFromPreviousUpload(prev[0]); upload.start(); });
```

- The same policies, size limits and mime rules as normal uploads apply. They're checked when the upload is created and again when it completes.
- Only the user who started an upload can continue it.
- The file appears in the bucket only when the last byte arrives. Unfinished uploads expire after 24 hours, and survive server restarts until then.
- On S3 backends, files over 100 MB use multipart uploads.

Not supported yet: signed resumable upload URLs (`/upload/resumable/sign`), `Upload-Defer-Length` and concatenation.

## Backups

Storage files are part of each project's backup bundle when they're on local disk. Files in your own S3 bucket stay there and are not copied into backups.
