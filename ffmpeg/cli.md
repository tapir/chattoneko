```sh
ffmpeg -nostdin -v error -y -threads 1 -filter_threads 1 -filter_complex_threads 1 -max_pixels 33177600 -i in -map 0:V:0 -vf "scale=w='min(iw,if(gte(iw,ih),1920,1080))':h='min(ih,if(gte(iw,ih),1920,1080))':force_original_aspect_ratio=decrease" -pix_fmt yuvj420p -q:v 3 -frames:v 1 out.jpg
ffmpeg -nostdin -v error -y -threads 1 -filter_threads 1 -filter_complex_threads 1 -i in -map 0:a:0 -map_metadata -1 -fflags +bitexact -ac 1 -ar 22050 -b:a 32k out.mp3
```
