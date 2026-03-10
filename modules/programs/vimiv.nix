{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.vimiv.enable = lib.mkEnableOption "enables vimiv" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.vimiv.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        vimiv-qt
      ];
      xdg.configFile = {
        "vimiv/vimiv.conf" = {
          text = ''
            [GENERAL]
            monitor_filesystem = True
            startup_library = True
            style = default-dark
            read_only = False

            [COMMAND]
            history_limit = 100

            [COMPLETION]
            fuzzy = False

            [SEARCH]
            ignore_case = True
            incremental = True

            [IMAGE]
            autoplay = True
            autowrite = ask
            overzoom = 1.0
            zoom_wheel_ctrl = True

            [LIBRARY]
            width = 0.3
            show_hidden = False

            [THUMBNAIL]
            size = 128
            save = True

            [SLIDESHOW]
            delay = 2.0
            indicator = slideshow:

            [STATUSBAR]
            collapse_home = True
            show = True
            message_timeout = 60000
            mark_indicator = <b>*</b>
            left = {pwd}{read-only}
            left_image = {index}/{total} {basename}{read-only} [{zoomlevel}]
            left_thumbnail = {thumbnail-index}/{thumbnail-total} {thumbnail-basename}{read-only}
            left_manipulate = {basename}   {image-size}   Modified: {modified}   {processing}
            center_thumbnail = {thumbnail-size}
            center = {slideshow-indicator} {slideshow-delay} {transformation-info}
            right = {keys}  {mark-count}  {mode}
            right_image = {keys}  {mark-indicator} {mark-count}  {mode}

            [KEYHINT]
            delay = 500
            timeout = 5000

            [TITLE]
            fallback = vimiv
            image = vimiv - {basename}

            [METADATA]
            keys1 = Exif.Image.Make,Exif.Image.Model,Exif.Photo.LensModel,Exif.Image.DateTime,Exif.Image.Artist,Exif.Image.Copyright
            keys2 = Exif.Photo.ExposureTime,Exif.Photo.FNumber,Exif.Photo.ISOSpeedRatings,Exif.Photo.ApertureValue,Exif.Photo.ExposureBiasValue,Exif.Photo.FocalLength,Exif.Photo.ExposureProgram
            keys3 = Exif.GPSInfo.GPSLatitudeRef,Exif.GPSInfo.GPSLatitude,Exif.GPSInfo.GPSLongitudeRef,Exif.GPSInfo.GPSLongitude,Exif.GPSInfo.GPSAltitudeRef,Exif.GPSInfo.GPSAltitude
            keys4 = Iptc.Application2.Caption,Iptc.Application2.Keywords,Iptc.Application2.City,Iptc.Application2.SubLocation,Iptc.Application2.ProvinceState,Iptc.Application2.CountryName,Iptc.Application2.Source,Iptc.Application2.Credit,Iptc.Application2.Copyright,Iptc.Application2.Contact
            keys5 = Exif.Image.ImageWidth,Exif.Image.ImageLength,Exif.Photo.PixelXDimension,Exif.Photo.PixelYDimension,Exif.Image.BitsPerSample,Exif.Image.Compression,Exif.Photo.ColorSpace

            [SORT]
            image_order = alphabetical
            directory_order = alphabetical
            reverse = False
            ignore_case = False
            shuffle = False

            [PLUGINS]
            print = default
            metadata = default

            [ALIASES]
          '';
        };
        "vimiv/keys.conf" = {
          text = ''
            [GLOBAL]
            <colon> : command
            o : command --text='open '
            yi : copy-image
            yI : copy-image --primary
            yy : copy-name
            ya : copy-name --abspath
            yA : copy-name --abspath --primary
            yY : copy-name --primary
            D : delete %%
            gi : enter image
            gl : enter library
            gm : enter manipulate
            gt : enter thumbnail
            f : fullscreen
            G : goto -1
            gg : goto 1
            m : mark %%
            q : quit
            . : repeat-command
            j : scroll down
            h : scroll left
            l : scroll right
            k : scroll up
            / : search
            ? : search --reverse
            N : search-next
            P : search-prev
            zh : set library.show_hidden!
            b : set statusbar.show!
            tl : toggle library
            tm : toggle manipulate
            tt : toggle thumbnail

            [IMAGE]
            M : center
            <button-right> : enter library
            <button-middle> : enter thumbnail
            | : flip
            _ : flip --vertical
            <end> : goto -1
            <home> : goto 1
            <button-forward> : next
            <page-down> : next
            n : next
            <ctrl>n : next --keep-zoom
            <space> : play-or-pause
            <button-back> : prev
            <page-up> : prev
            p : prev
            <ctrl>p : prev --keep-zoom
            > : rotate
            < : rotate --counter-clockwise
            W : scale --level=1
            <equal> : scale --level=fit
            w : scale --level=fit
            E : scale --level=fit-height
            e : scale --level=fit-width
            J : scroll-edge down
            H : scroll-edge left
            L : scroll-edge right
            K : scroll-edge up
            sl : set slideshow.delay +0.5
            sh : set slideshow.delay -0.5
            ss : slideshow
            + : zoom in
            - : zoom out

            [LIBRARY]
            <button-middle> : enter thumbnail
            go : goto 1 --open-selected
            <button-forward> : scroll down --open-selected
            n : scroll down --open-selected
            <ctrl>d : scroll half-page-down
            <ctrl>u : scroll half-page-up
            <button-right> : scroll left
            <ctrl>f : scroll page-down
            <ctrl>b : scroll page-up
            <button-back> : scroll up --open-selected
            p : scroll up --open-selected
            L : set library.width +0.05
            H : set library.width -0.05

            [THUMBNAIL]
            $ : end-of-line
            <button-right> : enter library
            ^ : first-of-line
            <ctrl>d : scroll half-page-down
            <ctrl>u : scroll half-page-up
            <button-back> : scroll left
            <ctrl>f : scroll page-down
            <ctrl>b : scroll page-up
            <button-forward> : scroll right
            + : zoom in
            - : zoom out

            [COMMAND]
            <tab> : complete
            <shift><tab> : complete --inverse
            <ctrl>p : history next
            <ctrl>n : history prev
            <up> : history-substr-search next
            <down> : history-substr-search prev
            <escape> : leave-commandline

            [MANIPULATE]
            <colon> : command
            f : fullscreen
            b : set statusbar.show!
          '';
        };
      };
      xdg.mimeApps.defaultApplications = {
        "image/jpeg" = [ "vimiv.desktop" ];
        "image/png" = [ "vimiv.desktop" ];
      };
    };
  };
}
