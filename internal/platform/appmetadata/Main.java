package top.e404.droidharbor.appmetadata;

import android.content.Context;
import android.content.pm.PackageInfo;
import android.content.pm.PackageManager;
import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.drawable.Drawable;
import android.os.Build;
import android.os.Looper;
import android.os.UserHandle;
import android.util.Base64;
import android.util.JsonWriter;

import java.io.ByteArrayOutputStream;
import java.io.OutputStreamWriter;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.HashSet;
import java.util.Set;

/** Reads only the requested Android user's package resources; never runs app code. */
public final class Main {
    private static final int ICON_SIZE = 40;

    public static void main(String[] args) {
        try {
            run(Integer.parseInt(args[0]), args[1].equals("render"),
                    new HashSet<>(Arrays.asList(args).subList(2, args.length)));
            System.exit(0);
        } catch (Exception failure) {
            System.err.println(failure.getClass().getSimpleName());
            System.exit(1);
        }
    }

    @SuppressWarnings("deprecation")
    private static void run(int user, boolean render, Set<String> requested) throws Exception {
        Looper.prepareMainLooper();
        Class<?> activityThread = Class.forName("android.app.ActivityThread");
        Object thread = activityThread.getMethod("systemMain").invoke(null);
        Context system = (Context) activityThread.getMethod("getSystemContext").invoke(thread);
        UserHandle handle = UserHandle.getUserHandleForUid(Math.multiplyExact(user, 100000));
        Context context = (Context) Context.class.getMethod("createContextAsUser", UserHandle.class, int.class)
                .invoke(system, handle, 0);
        PackageManager pm = context.getPackageManager();
        Bitmap bitmap = render ? Bitmap.createBitmap(ICON_SIZE, ICON_SIZE, Bitmap.Config.ARGB_8888) : null;
        Canvas canvas = render ? new Canvas(bitmap) : null;
        IconBuffer png = render ? new IconBuffer() : null;
        JsonWriter writer = new JsonWriter(new OutputStreamWriter(System.out, StandardCharsets.UTF_8));
        writer.beginObject().name("userId").value(user).name("apps").beginArray();
        for (PackageInfo pkg : pm.getInstalledPackages(0)) {
            if (!requested.contains(pkg.packageName)) continue;
            long version = Build.VERSION.SDK_INT >= 28 ? pkg.getLongVersionCode()
                    : (pkg.versionCode & 0xffffffffL);
            writer.beginObject().name("packageName").value(pkg.packageName).name("versionCode").value(version);
            if (render) {
                CharSequence label = pm.getApplicationLabel(pkg.applicationInfo);
                writer.name("name").value(label.toString());
                Drawable icon = pm.getApplicationIcon(pkg.applicationInfo);
                bitmap.eraseColor(Color.TRANSPARENT);
                icon.setBounds(0, 0, ICON_SIZE, ICON_SIZE);
                icon.draw(canvas);
                png.reset();
                if (!bitmap.compress(Bitmap.CompressFormat.PNG, 100, png)) {
                    throw new IllegalStateException("PNG encoder failed");
                }
                writer.name("icon").value(png.encode());
            }
            writer.endObject();
        }
        writer.endArray().endObject().flush();
    }

    private static final class IconBuffer extends ByteArrayOutputStream {
        String encode() {
            return Base64.encodeToString(buf, 0, count, Base64.NO_WRAP);
        }
    }
}
