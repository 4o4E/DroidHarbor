package top.e404.droidharbor.appinput;

import android.accessibilityservice.AccessibilityServiceInfo;
import android.app.UiAutomation;
import android.graphics.Rect;
import android.os.Bundle;
import android.os.HandlerThread;
import android.os.Looper;
import android.text.TextUtils;
import android.view.accessibility.AccessibilityNodeInfo;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.nio.charset.StandardCharsets;

/** Performs one edit on the observed, still-focused Android field. Runs as ADB shell, not an installed service. */
public final class Main {
    public static void main(String[] args) {
        HandlerThread callbacks = new HandlerThread("droidharbor-editor");
        callbacks.start();
        UiAutomation ui = null;
        boolean connected = false;
        int status = 0;
        try {
            BufferedReader input = new BufferedReader(new InputStreamReader(System.in, StandardCharsets.UTF_8));
            String before = input.readLine();
            String text = input.readLine();
            if (before == null || text == null || text.isEmpty()) {
                System.out.println("UNSUPPORTED");
            } else {
                // Use the same shell connection as Android's UI dumper.
                Class<?> connectionType = Class.forName("android.app.IUiAutomationConnection");
                Object connection = Class.forName("android.app.UiAutomationConnection").getConstructor().newInstance();
                ui = UiAutomation.class.getConstructor(Looper.class, connectionType).newInstance(callbacks.getLooper(), connection);
                UiAutomation.class.getMethod("connect", int.class).invoke(ui, UiAutomation.FLAG_DONT_SUPPRESS_ACCESSIBILITY_SERVICES);
                connected = true;
                AccessibilityServiceInfo info = ui.getServiceInfo();
                info.flags |= AccessibilityServiceInfo.FLAG_REPORT_VIEW_IDS;
                ui.setServiceInfo(info);
                edit(ui, args, before, text);
            }
        } catch (Exception failure) {
            System.err.println(failure.getClass().getSimpleName());
            if (!connected) {
                System.out.println("UNSUPPORTED");
            } else {
                status = 1;
            }
        } finally {
            try {
                if (connected) UiAutomation.class.getMethod("disconnect").invoke(ui);
            } catch (Exception failure) {
                System.err.println(failure.getClass().getSimpleName());
                status = 1;
            } finally {
                callbacks.quitSafely();
            }
        }
        System.exit(status);
    }

    private static void edit(UiAutomation ui, String[] args, String before, String text) {
        AccessibilityNodeInfo root = ui.getRootInActiveWindow();
        AccessibilityNodeInfo field = ui.findFocus(AccessibilityNodeInfo.FOCUS_INPUT);
        Rect bounds = new Rect();
        if (root == null || field == null || !TextUtils.equals(args[0], root.getPackageName()) ||
                root.getWindowId() != field.getWindowId() || !matches(field, args)) {
            System.out.println("NOT_SENT");
            return;
        }
        field.getBoundsInScreen(bounds);
        Rect expected = new Rect(Integer.parseInt(args[3]), Integer.parseInt(args[4]),
                Integer.parseInt(args[5]), Integer.parseInt(args[6]));
        if (!field.isVisibleToUser() || !bounds.equals(expected) || !matchesValue(field, before)) {
            System.out.println("NOT_SENT");
            return;
        }
        if (!field.getActionList().contains(AccessibilityNodeInfo.AccessibilityAction.ACTION_SET_TEXT)) {
            System.out.println("UNSUPPORTED");
            return;
        }
        if (field.isShowingHintText()) before = "";
        if (!before.isEmpty() && (field.getTextSelectionStart() != 0 || field.getTextSelectionEnd() != before.length())) {
            Bundle selection = new Bundle();
            selection.putInt(AccessibilityNodeInfo.ACTION_ARGUMENT_SELECTION_START_INT, 0);
            selection.putInt(AccessibilityNodeInfo.ACTION_ARGUMENT_SELECTION_END_INT, before.length());
            if (!field.performAction(AccessibilityNodeInfo.ACTION_SET_SELECTION, selection)) {
                System.out.println("SELECTION_UNVERIFIED");
                return;
            }
        }
        // A selection listener can move focus to another field or window.
        // The old node may still report focused within its own window.
        AccessibilityNodeInfo active = ui.getRootInActiveWindow();
        AccessibilityNodeInfo focused = ui.findFocus(AccessibilityNodeInfo.FOCUS_INPUT);
        if (active == null || !field.equals(focused) || active.getWindowId() != field.getWindowId() ||
                !focused.refresh() || !matches(focused, args) || !matchesValue(focused, before)) {
            System.out.println("NOT_SENT");
            return;
        }
        focused.getBoundsInScreen(bounds);
        if (!focused.isVisibleToUser() || !bounds.equals(expected)) {
            System.out.println("NOT_SENT");
            return;
        }
        if (!before.isEmpty() && (focused.getTextSelectionStart() != 0 || focused.getTextSelectionEnd() != before.length())) {
            System.out.println("SELECTION_UNVERIFIED");
            return;
        }
        Bundle replacement = new Bundle();
        replacement.putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, text);
        System.out.println(focused.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, replacement) ? "SENT" : "REJECTED");
    }

    private static boolean matches(AccessibilityNodeInfo field, String[] args) {
        return field.isFocused() && field.isEnabled() && field.isEditable() && !field.isPassword() &&
                TextUtils.equals(args[0], field.getPackageName()) && TextUtils.equals(args[1], field.getClassName()) &&
                TextUtils.equals(args[2], field.getViewIdResourceName() == null ? "" : field.getViewIdResourceName());
    }

    private static boolean matchesValue(AccessibilityNodeInfo field, String text) {
        CharSequence value = field.getText();
        if (field.isShowingHintText()) {
            return text.isEmpty() || TextUtils.equals(text, value);
        }
        return text.isEmpty() ? TextUtils.isEmpty(value) : TextUtils.equals(text, value);
    }
}
